package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/abinthomasonline/queueframe/internal/domain"
)

const queuedOrderColumns = `
id,trader_id,execution_date,position,tag,state,exchange,trading_symbol,
transaction_type,product,order_type,quantity,disclosed_quantity,price,
trigger_price,validity,market_protection_text,created_at_ms,updated_at_ms,deleted_at_ms`

func normalizeQueuedParams(params domain.OrderParams) (domain.OrderParams, error) {
	params.Exchange = strings.ToUpper(strings.TrimSpace(params.Exchange))
	params.TradingSymbol = strings.ToUpper(strings.TrimSpace(params.TradingSymbol))
	params.TransactionType = strings.ToUpper(strings.TrimSpace(params.TransactionType))
	params.Product = strings.ToUpper(strings.TrimSpace(params.Product))
	params.OrderType = strings.ToUpper(strings.TrimSpace(params.OrderType))
	if params.Exchange != "NSE" && params.Exchange != "BSE" {
		return domain.OrderParams{}, errors.New("exchange must be NSE or BSE")
	}
	if params.TradingSymbol == "" {
		return domain.OrderParams{}, errors.New("a validated trading symbol is required")
	}
	if params.TransactionType != "BUY" && params.TransactionType != "SELL" {
		return domain.OrderParams{}, errors.New("transaction type must be BUY or SELL")
	}
	if params.Product != "CNC" && params.Product != "MIS" {
		return domain.OrderParams{}, errors.New("product must be CNC or MIS")
	}
	if params.OrderType != "LIMIT" && params.OrderType != "MARKET" {
		return domain.OrderParams{}, errors.New("order type must be LIMIT or MARKET")
	}
	if params.Quantity <= 0 {
		return domain.OrderParams{}, errors.New("quantity must be positive")
	}
	params.DisclosedQuantity = 0
	params.TriggerPrice = ""
	params.Validity = "DAY"
	params.ValidityTTL = 0
	params.Autoslice = false
	if params.OrderType == "MARKET" {
		params.Price = ""
		params.MarketProtection = "-1"
	} else {
		if strings.TrimSpace(params.Price) == "" {
			return domain.OrderParams{}, errors.New("limit price is required")
		}
		params.Price = strings.TrimSpace(params.Price)
		params.MarketProtection = "0"
	}
	return params, nil
}

func (s *Store) CreateQueuedOrder(ctx context.Context, traderID int64, order domain.QueuedOrder, now time.Time) (domain.QueuedOrder, int64, error) {
	if traderID <= 0 {
		return domain.QueuedOrder{}, 0, ErrNotFound
	}
	if _, err := time.Parse("2006-01-02", order.ExecutionDate); err != nil {
		return domain.QueuedOrder{}, 0, errors.New("execution date must use YYYY-MM-DD")
	}
	params, err := normalizeQueuedParams(order.Params)
	if err != nil {
		return domain.QueuedOrder{}, 0, err
	}
	payload, err := json.Marshal(params)
	if err != nil {
		return domain.QueuedOrder{}, 0, err
	}
	tag, err := newQueuedOrderTag()
	if err != nil {
		return domain.QueuedOrder{}, 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.QueuedOrder{}, 0, err
	}
	defer tx.Rollback()
	settings, err := settingsForDateQuery(tx.QueryRowContext(ctx, `
SELECT id,version,effective_date,dispatch_clock,user_blackout_start,user_blackout_end,
       freeze_lead_ms,batch_interval_ms,max_orders_per_trader,
       COALESCE(created_by_admin_id,0),created_at_ms
FROM runtime_settings_versions WHERE effective_date<=?
ORDER BY effective_date DESC,version DESC LIMIT 1`, order.ExecutionDate))
	if err != nil {
		return domain.QueuedOrder{}, 0, err
	}
	if err := ensureQueueMutable(ctx, tx, order.ExecutionDate, settings, now); err != nil {
		return domain.QueuedOrder{}, 0, err
	}
	var traderExists int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM traders WHERE id=? AND deleted_at_ms=0)`, traderID).Scan(&traderExists); err != nil {
		return domain.QueuedOrder{}, 0, err
	}
	if traderExists == 0 {
		return domain.QueuedOrder{}, 0, ErrNotFound
	}
	var count int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM queued_orders
WHERE trader_id=? AND execution_date=? AND deleted_at_ms=0`, traderID, order.ExecutionDate).Scan(&count); err != nil {
		return domain.QueuedOrder{}, 0, err
	}
	if count >= settings.MaxOrdersPerTrader {
		return domain.QueuedOrder{}, 0, fmt.Errorf("maximum of %d orders per trader/date reached", settings.MaxOrdersPerTrader)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO trader_queue_versions(trader_id,execution_date,revision,created_at_ms,updated_at_ms)
VALUES(?,?,0,?,?) ON CONFLICT(trader_id,execution_date) DO NOTHING`, traderID, order.ExecutionDate, millis(now), millis(now)); err != nil {
		return domain.QueuedOrder{}, 0, err
	}
	position := count + 1
	result, err := tx.ExecContext(ctx, `
INSERT INTO queued_orders(
 trader_id,execution_date,position,tag,state,exchange,trading_symbol,transaction_type,
 product,order_type,quantity,disclosed_quantity,price,trigger_price,validity,
 market_protection_text,payload_json,created_at_ms,updated_at_ms)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, traderID, order.ExecutionDate, position, tag,
		domain.OrderStateScheduled, params.Exchange, params.TradingSymbol, params.TransactionType,
		params.Product, params.OrderType, params.Quantity, params.DisclosedQuantity, params.Price,
		params.TriggerPrice, "DAY", params.MarketProtection, string(payload), millis(now), millis(now))
	if err != nil {
		return domain.QueuedOrder{}, 0, err
	}
	order.ID, _ = result.LastInsertId()
	result, err = tx.ExecContext(ctx, `
UPDATE trader_queue_versions SET revision=revision+1,updated_at_ms=?
WHERE trader_id=? AND execution_date=?`, millis(now), traderID, order.ExecutionDate)
	if err != nil {
		return domain.QueuedOrder{}, 0, err
	}
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM trader_queue_versions WHERE trader_id=? AND execution_date=?`, traderID, order.ExecutionDate).Scan(&revision); err != nil {
		return domain.QueuedOrder{}, 0, err
	}
	order.TraderID = traderID
	order.Position = position
	order.Tag = tag
	order.State = domain.OrderStateScheduled
	order.Params = params
	order.CreatedAt, order.UpdatedAt = now, now
	if err := tx.Commit(); err != nil {
		return domain.QueuedOrder{}, 0, err
	}
	return order, revision, nil
}

func settingsForDateQuery(row scanner) (domain.RuntimeSettings, error) {
	return scanRuntimeSettings(row)
}

func ensureQueueMutable(ctx context.Context, tx *sql.Tx, executionDate string, settings domain.RuntimeSettings, now time.Time) error {
	var frozen int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM queued_date_freezes WHERE execution_date=?)`, executionDate).Scan(&frozen); err != nil {
		return err
	}
	if frozen != 0 {
		return ErrQueueFrozen
	}
	target, err := SettingsDispatchAt(settings, executionDate)
	if err != nil {
		return err
	}
	localDate := now.In(indiaLocation).Format("2006-01-02")
	if executionDate < localDate {
		return errors.New("execution date is in the past")
	}
	if !now.Before(target.Add(-settings.FreezeLead)) {
		return ErrQueueFrozen
	}
	return nil
}

func newQueuedOrderTag() (string, error) {
	tag, err := newOrderTag()
	if err != nil {
		return "", err
	}
	return "QF" + tag[2:], nil
}

func scanQueuedOrder(row scanner) (domain.QueuedOrder, error) {
	var order domain.QueuedOrder
	var createdMS, updatedMS, deletedMS int64
	err := row.Scan(&order.ID, &order.TraderID, &order.ExecutionDate, &order.Position, &order.Tag,
		&order.State, &order.Params.Exchange, &order.Params.TradingSymbol, &order.Params.TransactionType,
		&order.Params.Product, &order.Params.OrderType, &order.Params.Quantity, &order.Params.DisclosedQuantity,
		&order.Params.Price, &order.Params.TriggerPrice, &order.Params.Validity,
		&order.Params.MarketProtection, &createdMS, &updatedMS, &deletedMS)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.QueuedOrder{}, ErrNotFound
	}
	if err != nil {
		return domain.QueuedOrder{}, err
	}
	order.CreatedAt, order.UpdatedAt, order.DeletedAt = fromMillis(createdMS), fromMillis(updatedMS), fromMillis(deletedMS)
	return order, nil
}

func (s *Store) ListTraderOrders(ctx context.Context, traderID int64, executionDate string) (domain.OrderQueue, error) {
	queue := domain.OrderQueue{TraderID: traderID, ExecutionDate: executionDate}
	rows, err := s.db.QueryContext(ctx, `SELECT `+queuedOrderColumns+`
FROM queued_orders WHERE trader_id=? AND execution_date=? AND deleted_at_ms=0
ORDER BY position`, traderID, executionDate)
	if err != nil {
		return domain.OrderQueue{}, err
	}
	for rows.Next() {
		order, err := scanQueuedOrder(rows)
		if err != nil {
			rows.Close()
			return domain.OrderQueue{}, err
		}
		queue.Orders = append(queue.Orders, order)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return domain.OrderQueue{}, err
	}
	if err := rows.Close(); err != nil {
		return domain.OrderQueue{}, err
	}
	err = s.db.QueryRowContext(ctx, `
SELECT revision FROM trader_queue_versions WHERE trader_id=? AND execution_date=?`, traderID, executionDate).Scan(&queue.Revision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return domain.OrderQueue{}, err
	}
	var frozenMS int64
	err = s.db.QueryRowContext(ctx, `SELECT frozen_at_ms FROM queued_date_freezes WHERE execution_date=?`, executionDate).Scan(&frozenMS)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return domain.OrderQueue{}, err
	}
	queue.FrozenAt = fromMillis(frozenMS)
	return queue, nil
}

func (s *Store) ListAllQueuedOrders(ctx context.Context, executionDate string) ([]domain.QueuedOrder, error) {
	query := `SELECT ` + queuedOrderColumns + ` FROM queued_orders WHERE deleted_at_ms=0`
	args := []any{}
	if executionDate != "" {
		query += ` AND execution_date=?`
		args = append(args, executionDate)
	}
	query += ` ORDER BY execution_date DESC,trader_id,position`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var orders []domain.QueuedOrder
	for rows.Next() {
		order, err := scanQueuedOrder(rows)
		if err != nil {
			return nil, err
		}
		orders = append(orders, order)
	}
	return orders, rows.Err()
}

func (s *Store) QueuedOrder(ctx context.Context, traderID, orderID int64) (domain.QueuedOrder, error) {
	return scanQueuedOrder(s.db.QueryRowContext(ctx, `SELECT `+queuedOrderColumns+`
FROM queued_orders WHERE id=? AND trader_id=? AND deleted_at_ms=0`, orderID, traderID))
}

func (s *Store) QueuedOrderByID(ctx context.Context, orderID int64) (domain.QueuedOrder, error) {
	return scanQueuedOrder(s.db.QueryRowContext(ctx, `SELECT `+queuedOrderColumns+`
FROM queued_orders WHERE id=? AND deleted_at_ms=0`, orderID))
}

// UpdateQueuedOrder replaces only the user-editable broker parameters. Trader,
// date, position, tag, DAY validity, and internal market protection remain
// immutable. The queue revision prevents an admin edit from overwriting a
// trader reorder (and vice versa).
func (s *Store) UpdateQueuedOrder(ctx context.Context, traderID, orderID int64, expectedSourceDate string, expectedRevision int64, updated domain.QueuedOrder, now time.Time) (domain.QueuedOrder, int64, error) {
	params, err := normalizeQueuedParams(updated.Params)
	if err != nil {
		return domain.QueuedOrder{}, 0, err
	}
	payload, err := json.Marshal(params)
	if err != nil {
		return domain.QueuedOrder{}, 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.QueuedOrder{}, 0, err
	}
	defer tx.Rollback()
	var order domain.QueuedOrder
	var createdMS, updatedMS, deletedMS int64
	err = tx.QueryRowContext(ctx, `SELECT `+queuedOrderColumns+`
FROM queued_orders WHERE id=? AND trader_id=? AND deleted_at_ms=0`, orderID, traderID).Scan(
		&order.ID, &order.TraderID, &order.ExecutionDate, &order.Position, &order.Tag,
		&order.State, &order.Params.Exchange, &order.Params.TradingSymbol, &order.Params.TransactionType,
		&order.Params.Product, &order.Params.OrderType, &order.Params.Quantity, &order.Params.DisclosedQuantity,
		&order.Params.Price, &order.Params.TriggerPrice, &order.Params.Validity,
		&order.Params.MarketProtection, &createdMS, &updatedMS, &deletedMS)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.QueuedOrder{}, 0, ErrNotFound
	}
	if err != nil {
		return domain.QueuedOrder{}, 0, err
	}
	if order.State != domain.OrderStateScheduled {
		return domain.QueuedOrder{}, 0, ErrQueueFrozen
	}
	sourceDate := order.ExecutionDate
	if strings.TrimSpace(expectedSourceDate) != sourceDate {
		return domain.QueuedOrder{}, 0, ErrQueueRevisionConflict
	}
	destinationDate := strings.TrimSpace(updated.ExecutionDate)
	if destinationDate == "" {
		destinationDate = sourceDate
	}
	if _, err := time.Parse("2006-01-02", destinationDate); err != nil {
		return domain.QueuedOrder{}, 0, errors.New("execution date must use YYYY-MM-DD")
	}
	settings, err := settingsForDateQuery(tx.QueryRowContext(ctx, `
SELECT id,version,effective_date,dispatch_clock,user_blackout_start,user_blackout_end,
 freeze_lead_ms,batch_interval_ms,max_orders_per_trader,COALESCE(created_by_admin_id,0),created_at_ms
FROM runtime_settings_versions WHERE effective_date<=? ORDER BY effective_date DESC,version DESC LIMIT 1`, sourceDate))
	if err != nil {
		return domain.QueuedOrder{}, 0, err
	}
	if err := ensureQueueMutable(ctx, tx, sourceDate, settings, now); err != nil {
		return domain.QueuedOrder{}, 0, err
	}
	var revision int64
	if err := tx.QueryRowContext(ctx, `
SELECT revision FROM trader_queue_versions WHERE trader_id=? AND execution_date=?`, traderID, sourceDate).Scan(&revision); err != nil {
		return domain.QueuedOrder{}, 0, err
	}
	if revision != expectedRevision {
		return domain.QueuedOrder{}, 0, ErrQueueRevisionConflict
	}
	newRevision := expectedRevision + 1
	if destinationDate == sourceDate {
		result, err := tx.ExecContext(ctx, `
UPDATE queued_orders SET exchange=?,trading_symbol=?,transaction_type=?,product=?,order_type=?,
 quantity=?,disclosed_quantity=0,price=?,trigger_price='',validity='DAY',market_protection_text=?,
 payload_json=?,updated_at_ms=? WHERE id=? AND state=?`, params.Exchange, params.TradingSymbol,
			params.TransactionType, params.Product, params.OrderType, params.Quantity, params.Price,
			params.MarketProtection, string(payload), millis(now), orderID, domain.OrderStateScheduled)
		if err != nil {
			return domain.QueuedOrder{}, 0, err
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			if err != nil {
				return domain.QueuedOrder{}, 0, err
			}
			return domain.QueuedOrder{}, 0, ErrQueueFrozen
		}
		result, err = tx.ExecContext(ctx, `
UPDATE trader_queue_versions SET revision=revision+1,updated_at_ms=?
WHERE trader_id=? AND execution_date=? AND revision=?`, millis(now), traderID, sourceDate, expectedRevision)
		if err != nil {
			return domain.QueuedOrder{}, 0, err
		}
		changed, err = result.RowsAffected()
		if err != nil || changed != 1 {
			if err != nil {
				return domain.QueuedOrder{}, 0, err
			}
			return domain.QueuedOrder{}, 0, ErrQueueRevisionConflict
		}
	} else {
		destinationSettings, err := settingsForDateQuery(tx.QueryRowContext(ctx, `
SELECT id,version,effective_date,dispatch_clock,user_blackout_start,user_blackout_end,
 freeze_lead_ms,batch_interval_ms,max_orders_per_trader,COALESCE(created_by_admin_id,0),created_at_ms
FROM runtime_settings_versions WHERE effective_date<=? ORDER BY effective_date DESC,version DESC LIMIT 1`, destinationDate))
		if err != nil {
			return domain.QueuedOrder{}, 0, err
		}
		if err := ensureQueueMutable(ctx, tx, destinationDate, destinationSettings, now); err != nil {
			return domain.QueuedOrder{}, 0, err
		}
		var destinationCount int
		if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM queued_orders WHERE trader_id=? AND execution_date=? AND deleted_at_ms=0`, traderID, destinationDate).Scan(&destinationCount); err != nil {
			return domain.QueuedOrder{}, 0, err
		}
		if destinationCount >= destinationSettings.MaxOrdersPerTrader {
			return domain.QueuedOrder{}, 0, fmt.Errorf("maximum of %d orders per trader/date reached", destinationSettings.MaxOrdersPerTrader)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO trader_queue_versions(trader_id,execution_date,revision,created_at_ms,updated_at_ms)
VALUES(?,?,0,?,?) ON CONFLICT(trader_id,execution_date) DO NOTHING`, traderID, destinationDate, millis(now), millis(now)); err != nil {
			return domain.QueuedOrder{}, 0, err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE queued_orders SET position=-id WHERE id=? AND state=?`, orderID, domain.OrderStateScheduled); err != nil {
			return domain.QueuedOrder{}, 0, err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE queued_orders SET position=position-1,updated_at_ms=?
WHERE trader_id=? AND execution_date=? AND deleted_at_ms=0 AND position>?`, millis(now), traderID, sourceDate, order.Position); err != nil {
			return domain.QueuedOrder{}, 0, err
		}
		result, err := tx.ExecContext(ctx, `
UPDATE queued_orders SET execution_date=?,position=?,exchange=?,trading_symbol=?,transaction_type=?,
 product=?,order_type=?,quantity=?,disclosed_quantity=0,price=?,trigger_price='',validity='DAY',
 market_protection_text=?,payload_json=?,updated_at_ms=? WHERE id=? AND state=?`, destinationDate,
			destinationCount+1, params.Exchange, params.TradingSymbol, params.TransactionType, params.Product,
			params.OrderType, params.Quantity, params.Price, params.MarketProtection, string(payload), millis(now),
			orderID, domain.OrderStateScheduled)
		if err != nil {
			return domain.QueuedOrder{}, 0, err
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			if err != nil {
				return domain.QueuedOrder{}, 0, err
			}
			return domain.QueuedOrder{}, 0, ErrQueueFrozen
		}
		result, err = tx.ExecContext(ctx, `
UPDATE trader_queue_versions SET revision=revision+1,updated_at_ms=?
WHERE trader_id=? AND execution_date=? AND revision=?`, millis(now), traderID, sourceDate, expectedRevision)
		if err != nil {
			return domain.QueuedOrder{}, 0, err
		}
		changed, err = result.RowsAffected()
		if err != nil || changed != 1 {
			if err != nil {
				return domain.QueuedOrder{}, 0, err
			}
			return domain.QueuedOrder{}, 0, ErrQueueRevisionConflict
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE trader_queue_versions SET revision=revision+1,updated_at_ms=?
WHERE trader_id=? AND execution_date=?`, millis(now), traderID, destinationDate); err != nil {
			return domain.QueuedOrder{}, 0, err
		}
		if err := tx.QueryRowContext(ctx, `
SELECT revision FROM trader_queue_versions WHERE trader_id=? AND execution_date=?`, traderID, destinationDate).Scan(&newRevision); err != nil {
			return domain.QueuedOrder{}, 0, err
		}
		order.ExecutionDate = destinationDate
		order.Position = destinationCount + 1
	}
	order.Params = params
	order.CreatedAt, order.UpdatedAt = fromMillis(createdMS), now
	if err := tx.Commit(); err != nil {
		return domain.QueuedOrder{}, 0, err
	}
	return order, newRevision, nil
}

func (s *Store) ReorderQueuedOrders(ctx context.Context, traderID int64, executionDate string, expectedRevision int64, orderedIDs []int64, now time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	settings, err := settingsForDateQuery(tx.QueryRowContext(ctx, `
SELECT id,version,effective_date,dispatch_clock,user_blackout_start,user_blackout_end,
 freeze_lead_ms,batch_interval_ms,max_orders_per_trader,COALESCE(created_by_admin_id,0),created_at_ms
FROM runtime_settings_versions WHERE effective_date<=? ORDER BY effective_date DESC,version DESC LIMIT 1`, executionDate))
	if err != nil {
		return 0, err
	}
	if err := ensureQueueMutable(ctx, tx, executionDate, settings, now); err != nil {
		return 0, err
	}
	var revision int64
	if err := tx.QueryRowContext(ctx, `
SELECT revision FROM trader_queue_versions WHERE trader_id=? AND execution_date=?`, traderID, executionDate).Scan(&revision); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	if revision != expectedRevision {
		return 0, ErrQueueRevisionConflict
	}
	rows, err := tx.QueryContext(ctx, `
SELECT id,state FROM queued_orders WHERE trader_id=? AND execution_date=? AND deleted_at_ms=0 ORDER BY position`, traderID, executionDate)
	if err != nil {
		return 0, err
	}
	existing := make(map[int64]bool)
	for rows.Next() {
		var id int64
		var state string
		if err := rows.Scan(&id, &state); err != nil {
			rows.Close()
			return 0, err
		}
		if state != domain.OrderStateScheduled {
			rows.Close()
			return 0, ErrQueueFrozen
		}
		existing[id] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	if len(existing) != len(orderedIDs) {
		return 0, errors.New("reorder must include every order exactly once")
	}
	seen := make(map[int64]bool, len(orderedIDs))
	for _, id := range orderedIDs {
		if !existing[id] || seen[id] {
			return 0, errors.New("reorder contains an unknown or duplicate order")
		}
		seen[id] = true
	}
	// Move all active rows outside the positive position namespace before
	// assigning the new dense order, avoiding transient unique violations.
	if _, err := tx.ExecContext(ctx, `
UPDATE queued_orders SET position=-id WHERE trader_id=? AND execution_date=? AND deleted_at_ms=0`, traderID, executionDate); err != nil {
		return 0, err
	}
	for index, id := range orderedIDs {
		if _, err := tx.ExecContext(ctx, `UPDATE queued_orders SET position=?,updated_at_ms=? WHERE id=?`, index+1, millis(now), id); err != nil {
			return 0, err
		}
	}
	result, err := tx.ExecContext(ctx, `
UPDATE trader_queue_versions SET revision=revision+1,updated_at_ms=?
WHERE trader_id=? AND execution_date=? AND revision=?`, millis(now), traderID, executionDate, expectedRevision)
	if err != nil {
		return 0, err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		if err != nil {
			return 0, err
		}
		return 0, ErrQueueRevisionConflict
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return expectedRevision + 1, nil
}

func (s *Store) DeleteQueuedOrder(ctx context.Context, traderID, orderID, expectedRevision int64, now time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var executionDate, state string
	var position int
	if err := tx.QueryRowContext(ctx, `
SELECT execution_date,position,state FROM queued_orders
WHERE id=? AND trader_id=? AND deleted_at_ms=0`, orderID, traderID).Scan(&executionDate, &position, &state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	if state != domain.OrderStateScheduled {
		return 0, ErrQueueFrozen
	}
	settings, err := settingsForDateQuery(tx.QueryRowContext(ctx, `
SELECT id,version,effective_date,dispatch_clock,user_blackout_start,user_blackout_end,
 freeze_lead_ms,batch_interval_ms,max_orders_per_trader,COALESCE(created_by_admin_id,0),created_at_ms
FROM runtime_settings_versions WHERE effective_date<=? ORDER BY effective_date DESC,version DESC LIMIT 1`, executionDate))
	if err != nil {
		return 0, err
	}
	if err := ensureQueueMutable(ctx, tx, executionDate, settings, now); err != nil {
		return 0, err
	}
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM trader_queue_versions WHERE trader_id=? AND execution_date=?`, traderID, executionDate).Scan(&revision); err != nil {
		return 0, err
	}
	if revision != expectedRevision {
		return 0, ErrQueueRevisionConflict
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE queued_orders SET state=?,position=-id,deleted_at_ms=?,updated_at_ms=? WHERE id=?`, domain.OrderStateDeleted, millis(now), millis(now), orderID); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE queued_orders SET position=position-1,updated_at_ms=?
WHERE trader_id=? AND execution_date=? AND deleted_at_ms=0 AND position>?`, millis(now), traderID, executionDate, position); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `
UPDATE trader_queue_versions SET revision=revision+1,updated_at_ms=?
WHERE trader_id=? AND execution_date=? AND revision=?`, millis(now), traderID, executionDate, expectedRevision)
	if err != nil {
		return 0, err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		if err != nil {
			return 0, err
		}
		return 0, ErrQueueRevisionConflict
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return expectedRevision + 1, nil
}

func (s *Store) FreezeQueuedDate(ctx context.Context, executionDate string, now time.Time) error {
	if _, err := time.Parse("2006-01-02", executionDate); err != nil {
		return errors.New("execution date must use YYYY-MM-DD")
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO queued_date_freezes(execution_date,frozen_at_ms) VALUES(?,?)
ON CONFLICT(execution_date) DO NOTHING`, executionDate, millis(now))
	return err
}

func (s *Store) QueuedCandidates(ctx context.Context, executionDate string) ([]domain.QueueCandidate, error) {
	return s.queuedCandidatesInState(ctx, executionDate, domain.OrderStateScheduled, false)
}

func (s *Store) UnknownQueuedCandidates(ctx context.Context, executionDate string) ([]domain.QueueCandidate, error) {
	// Keep deleted traders visible only for restart reconciliation. Their
	// credentials have already been scrubbed, so the scheduler can preserve the
	// durable UNKNOWN outcome without creating a broker client or resubmitting.
	return s.queuedCandidatesInState(ctx, executionDate, domain.OrderStateUnknown, true)
}

func (s *Store) queuedCandidatesInState(ctx context.Context, executionDate, state string, includeDeletedTraders bool) ([]domain.QueueCandidate, error) {
	qualifiedOrderColumns := "q." + strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(queuedOrderColumns), "\n", ""), ",", ",q.")
	traderFilter := " AND t.deleted_at_ms=0"
	if includeDeletedTraders {
		traderFilter = ""
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT `+qualifiedOrderColumns+`,
       t.id,t.client_id,t.display_client_id,t.api_key,t.api_secret_enc,
       COALESCE(a.access_token_enc,''),t.verified_at_ms,COALESCE(a.login_at_ms,0),
       COALESCE(a.expires_at_ms,0),CASE WHEN a.trader_id IS NULL THEN '' ELSE a.execution_date END,
       t.created_at_ms,t.updated_at_ms,t.deleted_at_ms
FROM queued_orders q JOIN traders t ON t.id=q.trader_id
LEFT JOIN trader_daily_activations a ON a.trader_id=t.id AND a.execution_date=q.execution_date
WHERE q.execution_date=? AND q.state=? AND q.deleted_at_ms=0`+traderFilter+`
ORDER BY q.trader_id,q.position`, executionDate, state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []domain.QueueCandidate
	for rows.Next() {
		var candidate domain.QueueCandidate
		var orderCreatedMS, orderUpdatedMS, orderDeletedMS int64
		var secretEnc, tokenEnc string
		var verifiedMS, loginMS, expiresMS, traderCreatedMS, traderUpdatedMS, traderDeletedMS int64
		err := rows.Scan(&candidate.Order.ID, &candidate.Order.TraderID, &candidate.Order.ExecutionDate,
			&candidate.Order.Position, &candidate.Order.Tag, &candidate.Order.State,
			&candidate.Order.Params.Exchange, &candidate.Order.Params.TradingSymbol,
			&candidate.Order.Params.TransactionType, &candidate.Order.Params.Product,
			&candidate.Order.Params.OrderType, &candidate.Order.Params.Quantity,
			&candidate.Order.Params.DisclosedQuantity, &candidate.Order.Params.Price,
			&candidate.Order.Params.TriggerPrice, &candidate.Order.Params.Validity,
			&candidate.Order.Params.MarketProtection, &orderCreatedMS, &orderUpdatedMS, &orderDeletedMS,
			&candidate.Trader.ID, &candidate.Trader.ClientID, &candidate.Trader.DisplayClientID,
			&candidate.Trader.APIKey, &secretEnc, &tokenEnc, &verifiedMS, &loginMS, &expiresMS,
			&candidate.Trader.ActivatedForDate, &traderCreatedMS, &traderUpdatedMS, &traderDeletedMS)
		if err != nil {
			return nil, err
		}
		candidate.Order.CreatedAt, candidate.Order.UpdatedAt, candidate.Order.DeletedAt = fromMillis(orderCreatedMS), fromMillis(orderUpdatedMS), fromMillis(orderDeletedMS)
		candidate.Trader.APISecret, err = s.vault.Decrypt(secretEnc)
		if err != nil {
			return nil, err
		}
		candidate.Trader.AccessToken, err = s.vault.Decrypt(tokenEnc)
		if err != nil {
			return nil, err
		}
		candidate.Trader.VerifiedAt, candidate.Trader.SessionLoginAt, candidate.Trader.SessionExpiresAt = fromMillis(verifiedMS), fromMillis(loginMS), fromMillis(expiresMS)
		candidate.Trader.CreatedAt, candidate.Trader.UpdatedAt, candidate.Trader.DeletedAt = fromMillis(traderCreatedMS), fromMillis(traderUpdatedMS), fromMillis(traderDeletedMS)
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

func sortedPlannedIDs(planned map[int64]time.Time) []int64 {
	ids := make([]int64, 0, len(planned))
	for id := range planned {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func (s *Store) PrepareQueuedOrders(ctx context.Context, executionDate string, planned map[int64]time.Time, now time.Time) (map[int64]bool, error) {
	return s.transitionScheduledQueuedOrders(ctx, executionDate, planned, now, domain.OrderStatePrepared, domain.OrderStatePrepared, "", "")
}

func (s *Store) MarkInactiveQueuedOrders(ctx context.Context, executionDate string, planned map[int64]time.Time, now time.Time) (map[int64]bool, error) {
	return s.transitionScheduledQueuedOrders(ctx, executionDate, planned, now, domain.OrderStateMissedInactive,
		domain.OrderStateMissedInactive, "InactiveSession", "Kite login was not completed within the same-day activation window before queue preparation")
}

func (s *Store) MarkMissedQueuedOrders(ctx context.Context, executionDate string, planned map[int64]time.Time, now time.Time, reason string) (map[int64]bool, error) {
	return s.transitionScheduledQueuedOrders(ctx, executionDate, planned, now, domain.OrderStateMissed,
		domain.OrderStateMissed, "Deadline", reason)
}

func (s *Store) transitionScheduledQueuedOrders(ctx context.Context, executionDate string, planned map[int64]time.Time, now time.Time, state, outcome, errorType, message string) (map[int64]bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO queued_date_freezes(execution_date,frozen_at_ms) VALUES(?,?)
ON CONFLICT(execution_date) DO NOTHING`, executionDate, millis(now)); err != nil {
		return nil, err
	}
	changedIDs := make(map[int64]bool, len(planned))
	for _, id := range sortedPlannedIDs(planned) {
		target := planned[id]
		if target.IsZero() {
			return nil, fmt.Errorf("queued order %d has no planned target", id)
		}
		var traderID int64
		var position int
		var payload, displayClientID string
		err := tx.QueryRowContext(ctx, `
SELECT q.trader_id,q.position,q.payload_json,t.display_client_id
FROM queued_orders q JOIN traders t ON t.id=q.trader_id
WHERE q.id=? AND q.execution_date=? AND q.deleted_at_ms=0 AND q.state=?`, id, executionDate, domain.OrderStateScheduled).
			Scan(&traderID, &position, &payload, &displayClientID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		result, err := tx.ExecContext(ctx, `
UPDATE queued_orders SET state=?,updated_at_ms=? WHERE id=? AND state=?`, state, millis(now), id, domain.OrderStateScheduled)
		if err != nil {
			return nil, err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if rows != 1 {
			continue
		}
		_, err = tx.ExecContext(ctx, `
INSERT INTO queued_dispatch_records(
 queued_order_id,trader_id,client_id_redacted,execution_date,queue_position,batch_number,
 payload_json,request_count,target_at_ms,outcome,error_type,error_message,created_at_ms,updated_at_ms)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, traderID, redactClientID(displayClientID), executionDate,
			position, (position-1)/10+1, payload, 0, millis(target), outcome, errorType, message, millis(now), millis(now))
		if err != nil {
			return nil, err
		}
		changedIDs[id] = true
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return changedIDs, nil
}

func (s *Store) RejectQueuedPreflight(ctx context.Context, orderID int64, plannedAt time.Time, errorType, message string, now time.Time) error {
	if plannedAt.IsZero() {
		return errors.New("planned dispatch time is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var traderID int64
	var executionDate, payload, displayClientID string
	var position int
	err = tx.QueryRowContext(ctx, `
SELECT q.trader_id,q.execution_date,q.position,q.payload_json,t.display_client_id
FROM queued_orders q JOIN traders t ON t.id=q.trader_id
WHERE q.id=? AND q.deleted_at_ms=0 AND q.state=?`, orderID, domain.OrderStateScheduled).
		Scan(&traderID, &executionDate, &position, &payload, &displayClientID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO queued_date_freezes(execution_date,frozen_at_ms) VALUES(?,?)
ON CONFLICT(execution_date) DO NOTHING`, executionDate, millis(now)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE queued_orders SET state=?,updated_at_ms=? WHERE id=?`, domain.OrderStateRejected, millis(now), orderID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO queued_dispatch_records(
 queued_order_id,trader_id,client_id_redacted,execution_date,queue_position,batch_number,payload_json,
 request_count,target_at_ms,outcome,error_type,error_message,created_at_ms,updated_at_ms)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, orderID, traderID, redactClientID(displayClientID), executionDate,
		position, (position-1)/10+1, payload, 0, millis(plannedAt), domain.OrderStateRejected,
		errorType, message, millis(now), millis(now)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ApplyQueuedDispatchResults(ctx context.Context, results []domain.DispatchResult) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, result := range results {
		if result.RequestCount < 0 || result.RequestCount > 1 {
			return fmt.Errorf("queued order %d request count must be zero or one", result.IntentID)
		}
		state := result.Outcome
		if state == "RATE_LIMITED" {
			state = domain.OrderStateRejected
		}
		var startedNS, finishedNS int64
		if !result.StartedAt.IsZero() {
			startedNS = result.StartedAt.UnixNano()
		}
		if !result.FinishedAt.IsZero() {
			finishedNS = result.FinishedAt.UnixNano()
		}
		update, err := tx.ExecContext(ctx, `
UPDATE queued_dispatch_records SET request_count=?,started_at_ns=?,finished_at_ns=?,
 order_timestamp_ms=?,exchange_timestamp_ms=?,exchange_update_timestamp_ms=?,outcome=?,
 order_id=?,exchange_order_id=?,broker_status=?,child_orders=?,error_type=?,error_message=?,updated_at_ms=?
WHERE queued_order_id=? AND (?=0 OR trader_id=?)`, result.RequestCount, startedNS, finishedNS,
			millis(result.OrderTimestamp), millis(result.ExchangeTimestamp), millis(result.ExchangeUpdateTimestamp),
			result.Outcome, result.OrderID, result.ExchangeOrderID, result.BrokerStatus, result.ChildOrders,
			result.ErrorType, result.ErrorMessage, millis(time.Now()), result.IntentID, result.AccountID, result.AccountID)
		if err != nil {
			return err
		}
		changed, err := update.RowsAffected()
		if err != nil || changed != 1 {
			if err != nil {
				return err
			}
			return fmt.Errorf("dispatch record for queued order %d was not found", result.IntentID)
		}
		var dispatchRecordID int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM queued_dispatch_records WHERE queued_order_id=?`, result.IntentID).Scan(&dispatchRecordID); err != nil {
			return err
		}
		for _, receipt := range result.Receipts {
			if !validDispatchReceiptSource(receipt.Source) {
				return fmt.Errorf("queued order %d has invalid receipt source %q", result.IntentID, receipt.Source)
			}
			if receipt.ObservedAt.IsZero() {
				return fmt.Errorf("queued order %d has a receipt without an observation time", result.IntentID)
			}
			if _, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO queued_dispatch_receipts(
 dispatch_record_id,source,observed_at_ns,order_id,exchange_order_id,broker_status,status_message,
 order_timestamp_ms,exchange_timestamp_ms,exchange_update_timestamp_ms)
VALUES(?,?,?,?,?,?,?,?,?,?)`, dispatchRecordID, receipt.Source, receipt.ObservedAt.UnixNano(),
				receipt.OrderID, receipt.ExchangeOrderID, receipt.BrokerStatus, receipt.StatusMessage,
				millis(receipt.OrderTimestamp), millis(receipt.ExchangeTimestamp), millis(receipt.ExchangeUpdateTimestamp)); err != nil {
				return err
			}
		}
		update, err = tx.ExecContext(ctx, `
UPDATE queued_orders SET state=?,updated_at_ms=?
WHERE id=? AND (state IN (?,?,?) OR state=?)`, state, millis(time.Now()), result.IntentID,
			domain.OrderStatePrepared, domain.OrderStateUnknown, domain.OrderStateAcknowledged, state)
		if err != nil {
			return err
		}
		changed, err = update.RowsAffected()
		if err != nil || changed != 1 {
			if err != nil {
				return err
			}
			return fmt.Errorf("queued order %d was not found while applying dispatch result", result.IntentID)
		}
	}
	return tx.Commit()
}

func validDispatchReceiptSource(source string) bool {
	switch source {
	case domain.DispatchReceiptKiteResponse, domain.DispatchReceiptOrderBook, domain.DispatchReceiptCurrentSnapshot:
		return true
	default:
		return false
	}
}

func (s *Store) RecoverPreparedQueuedOrders(ctx context.Context, executionDate string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
SELECT q.id,d.target_at_ms FROM queued_orders q JOIN queued_dispatch_records d ON d.queued_order_id=q.id
WHERE q.execution_date=? AND q.state=? AND d.outcome=? AND d.started_at_ns=0`, executionDate,
		domain.OrderStatePrepared, domain.OrderStatePrepared)
	if err != nil {
		return err
	}
	targets := make(map[int64]int64)
	for rows.Next() {
		var id, targetMS int64
		if err := rows.Scan(&id, &targetMS); err != nil {
			rows.Close()
			return err
		}
		targets[id] = targetMS
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for id, targetMS := range targets {
		if now.Before(fromMillis(targetMS)) {
			if _, err := tx.ExecContext(ctx, `DELETE FROM queued_dispatch_records WHERE queued_order_id=? AND outcome=? AND started_at_ns=0`, id, domain.OrderStatePrepared); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE queued_orders SET state=?,updated_at_ms=? WHERE id=? AND state=?`, domain.OrderStateScheduled, millis(now), id, domain.OrderStatePrepared); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `
UPDATE queued_dispatch_records SET outcome=?,error_type='Restart',
 error_message='process stopped before the dispatch result was persisted',updated_at_ms=?
WHERE queued_order_id=? AND outcome=?`, domain.OrderStateUnknown, millis(now), id, domain.OrderStatePrepared); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE queued_orders SET state=?,updated_at_ms=? WHERE id=? AND state=?`, domain.OrderStateUnknown, millis(now), id, domain.OrderStatePrepared); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Store) StaleQueuedDates(ctx context.Context, beforeDate string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT DISTINCT execution_date FROM queued_orders
WHERE execution_date<? AND deleted_at_ms=0 AND state IN (?,?) ORDER BY execution_date`,
		beforeDate, domain.OrderStateScheduled, domain.OrderStatePrepared)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var dates []string
	for rows.Next() {
		var date string
		if err := rows.Scan(&date); err != nil {
			return nil, err
		}
		dates = append(dates, date)
	}
	return dates, rows.Err()
}

func (s *Store) UnknownQueuedResults(ctx context.Context, executionDate string) ([]domain.DispatchResult, error) {
	rows, err := s.db.QueryContext(ctx, queuedDispatchResultQuery+`
WHERE d.execution_date=? AND d.outcome=? ORDER BY d.trader_id,d.queue_position`, executionDate, domain.OrderStateUnknown)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanQueuedDispatchResults(rows)
}

const queuedDispatchResultQuery = `
SELECT d.queued_order_id,d.trader_id,q.tag,1,d.request_count,d.target_at_ms,d.started_at_ns,d.finished_at_ns,
       d.order_timestamp_ms,d.exchange_timestamp_ms,d.exchange_update_timestamp_ms,d.outcome,
       d.order_id,d.exchange_order_id,d.broker_status,d.child_orders,d.error_type,d.error_message
FROM queued_dispatch_records d JOIN queued_orders q ON q.id=d.queued_order_id `

func scanQueuedDispatchResults(rows *sql.Rows) ([]domain.DispatchResult, error) {
	var results []domain.DispatchResult
	for rows.Next() {
		var result domain.DispatchResult
		var targetMS, startedNS, finishedNS, orderMS, exchangeMS, exchangeUpdateMS int64
		if err := rows.Scan(&result.IntentID, &result.AccountID, &result.Tag, &result.Attempt,
			&result.RequestCount, &targetMS, &startedNS, &finishedNS, &orderMS, &exchangeMS,
			&exchangeUpdateMS, &result.Outcome, &result.OrderID, &result.ExchangeOrderID,
			&result.BrokerStatus, &result.ChildOrders, &result.ErrorType, &result.ErrorMessage); err != nil {
			return nil, err
		}
		result.TargetAt = fromMillis(targetMS)
		if startedNS != 0 {
			result.StartedAt = time.Unix(0, startedNS)
		}
		if finishedNS != 0 {
			result.FinishedAt = time.Unix(0, finishedNS)
		}
		result.OrderTimestamp, result.ExchangeTimestamp, result.ExchangeUpdateTimestamp = fromMillis(orderMS), fromMillis(exchangeMS), fromMillis(exchangeUpdateMS)
		results = append(results, result)
	}
	return results, rows.Err()
}

func (s *Store) ListTraderDispatches(ctx context.Context, traderID int64, executionDate string) ([]domain.DispatchRecord, error) {
	return s.listQueuedDispatches(ctx, `d.trader_id=? AND (?='' OR d.execution_date=?)`, traderID, executionDate, executionDate)
}

func (s *Store) ListAllDispatches(ctx context.Context, executionDate string) ([]domain.DispatchRecord, error) {
	return s.listQueuedDispatches(ctx, `(?='' OR d.execution_date=?)`, executionDate, executionDate)
}

func (s *Store) listQueuedDispatches(ctx context.Context, where string, args ...any) ([]domain.DispatchRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT d.id,d.queued_order_id,d.trader_id,d.client_id_redacted,d.execution_date,d.queue_position,
       d.batch_number,d.payload_json,d.request_count,d.target_at_ms,d.started_at_ns,d.finished_at_ns,
       d.order_timestamp_ms,d.exchange_timestamp_ms,d.exchange_update_timestamp_ms,d.outcome,
       d.order_id,d.exchange_order_id,d.broker_status,d.child_orders,d.error_type,d.error_message,
       d.created_at_ms,d.updated_at_ms,q.tag
FROM queued_dispatch_records d JOIN queued_orders q ON q.id=d.queued_order_id
WHERE `+where+` ORDER BY d.execution_date DESC,d.trader_id,d.queue_position`, args...)
	if err != nil {
		return nil, err
	}
	var records []domain.DispatchRecord
	for rows.Next() {
		var record domain.DispatchRecord
		var targetMS, startedNS, finishedNS, orderMS, exchangeMS, exchangeUpdateMS, createdMS, updatedMS int64
		if err := rows.Scan(&record.ID, &record.QueuedOrderID, &record.TraderID, &record.ClientID,
			&record.ExecutionDate, &record.Position, &record.Batch, &record.PayloadJSON,
			&record.Result.RequestCount, &targetMS, &startedNS, &finishedNS, &orderMS, &exchangeMS,
			&exchangeUpdateMS, &record.Result.Outcome, &record.Result.OrderID,
			&record.Result.ExchangeOrderID, &record.Result.BrokerStatus, &record.Result.ChildOrders,
			&record.Result.ErrorType, &record.Result.ErrorMessage, &createdMS, &updatedMS,
			&record.Result.Tag); err != nil {
			return nil, err
		}
		record.Result.IntentID, record.Result.AccountID, record.Result.Attempt = record.QueuedOrderID, record.TraderID, 1
		record.Result.TargetAt = fromMillis(targetMS)
		if startedNS != 0 {
			record.Result.StartedAt = time.Unix(0, startedNS)
		}
		if finishedNS != 0 {
			record.Result.FinishedAt = time.Unix(0, finishedNS)
		}
		record.Result.OrderTimestamp, record.Result.ExchangeTimestamp, record.Result.ExchangeUpdateTimestamp = fromMillis(orderMS), fromMillis(exchangeMS), fromMillis(exchangeUpdateMS)
		record.CreatedAt, record.UpdatedAt = fromMillis(createdMS), fromMillis(updatedMS)
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return records, nil
	}
	receiptRows, err := s.db.QueryContext(ctx, `
SELECT r.id,r.dispatch_record_id,r.source,r.observed_at_ns,r.order_id,r.exchange_order_id,
       r.broker_status,r.status_message,r.order_timestamp_ms,r.exchange_timestamp_ms,
       r.exchange_update_timestamp_ms
FROM queued_dispatch_receipts r
JOIN queued_dispatch_records d ON d.id=r.dispatch_record_id
WHERE `+where+`
ORDER BY r.dispatch_record_id,r.observed_at_ns,r.id`, args...)
	if err != nil {
		return nil, err
	}
	defer receiptRows.Close()
	recordIndex := make(map[int64]int, len(records))
	for index := range records {
		recordIndex[records[index].ID] = index
	}
	for receiptRows.Next() {
		var receipt domain.DispatchReceipt
		var dispatchRecordID, observedNS, orderMS, exchangeMS, exchangeUpdateMS int64
		if err := receiptRows.Scan(&receipt.ID, &dispatchRecordID, &receipt.Source, &observedNS,
			&receipt.OrderID, &receipt.ExchangeOrderID, &receipt.BrokerStatus, &receipt.StatusMessage,
			&orderMS, &exchangeMS, &exchangeUpdateMS); err != nil {
			return nil, err
		}
		receipt.ObservedAt = time.Unix(0, observedNS)
		receipt.OrderTimestamp = fromMillis(orderMS)
		receipt.ExchangeTimestamp = fromMillis(exchangeMS)
		receipt.ExchangeUpdateTimestamp = fromMillis(exchangeUpdateMS)
		index, ok := recordIndex[dispatchRecordID]
		if !ok {
			return nil, fmt.Errorf("receipt references unlisted dispatch record %d", dispatchRecordID)
		}
		records[index].Receipts = append(records[index].Receipts, receipt)
	}
	if err := receiptRows.Err(); err != nil {
		return nil, err
	}
	for index := range records {
		records[index].Result.Receipts = append([]domain.DispatchReceipt(nil), records[index].Receipts...)
	}
	return records, nil
}
