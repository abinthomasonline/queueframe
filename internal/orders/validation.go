package orders

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/abinthomasonline/queueframe/internal/domain"
)

var decimalPattern = regexp.MustCompile(`^(?:[0-9]+(?:\.[0-9]+)?|\.[0-9]+)$`)

func Normalize(params domain.OrderParams) domain.OrderParams {
	params.Exchange = strings.ToUpper(strings.TrimSpace(params.Exchange))
	params.TradingSymbol = strings.ToUpper(strings.TrimSpace(params.TradingSymbol))
	params.TransactionType = strings.ToUpper(strings.TrimSpace(params.TransactionType))
	params.Product = strings.ToUpper(strings.TrimSpace(params.Product))
	params.OrderType = strings.ToUpper(strings.TrimSpace(params.OrderType))
	params.Validity = strings.ToUpper(strings.TrimSpace(params.Validity))
	params.Price = strings.TrimSpace(params.Price)
	params.TriggerPrice = strings.TrimSpace(params.TriggerPrice)
	params.MarketProtection = strings.TrimSpace(params.MarketProtection)
	return params
}

func ValidateParams(params domain.OrderParams) error {
	params = Normalize(params)
	if !oneOf(params.Exchange, "NSE", "BSE") {
		return errors.New("exchange must be NSE or BSE for scheduled equity dispatch")
	}
	if params.TradingSymbol == "" {
		return errors.New("trading symbol is required")
	}
	if !oneOf(params.TransactionType, "BUY", "SELL") {
		return errors.New("transaction type must be BUY or SELL")
	}
	if !oneOf(params.Product, "CNC", "MIS") {
		return errors.New("product must be CNC or MIS")
	}
	if !oneOf(params.OrderType, "LIMIT", "MARKET") {
		return errors.New("scheduled equity dispatch supports LIMIT or MARKET orders only")
	}
	if params.Validity != "DAY" {
		return errors.New("scheduled orders must use DAY validity")
	}
	if params.ValidityTTL != 0 {
		return errors.New("validity TTL must be 0 (omitted) for DAY orders")
	}
	if params.Quantity <= 0 {
		return errors.New("quantity must be positive")
	}
	if params.DisclosedQuantity < 0 || params.DisclosedQuantity > params.Quantity {
		return errors.New("disclosed quantity must be between zero and quantity")
	}
	if params.Autoslice {
		return errors.New("autoslice is disabled; submit one unsliced order")
	}
	if err := optionalDecimal("price", params.Price); err != nil {
		return err
	}
	if err := optionalDecimal("trigger price", params.TriggerPrice); err != nil {
		return err
	}
	if params.OrderType == "LIMIT" {
		if err := positiveDecimal("price", params.Price); err != nil {
			return err
		}
		if err := optionalDecimal("market protection", params.MarketProtection); err != nil || !isZeroDecimal(params.MarketProtection) {
			return errors.New("market protection must be 0 (omitted) for LIMIT orders")
		}
	} else {
		if !isZeroDecimal(params.Price) {
			return errors.New("price must be empty or 0 for MARKET orders")
		}
		if err := marketProtection(params.MarketProtection); err != nil {
			return err
		}
	}
	if !isZeroDecimal(params.TriggerPrice) {
		return errors.New("trigger price must be empty or 0 for scheduled orders")
	}
	return nil
}

func marketProtection(value string) error {
	if value == "-1" {
		return nil
	}
	if err := positiveDecimal("market protection", value); err != nil {
		return errors.New("market protection must be -1 (automatic) or a decimal percentage greater than 0 and up to 100 for MARKET orders")
	}
	parsed, ok := new(big.Rat).SetString(value)
	if !ok || parsed.Cmp(big.NewRat(100, 1)) > 0 {
		return errors.New("market protection must be -1 (automatic) or a decimal percentage greater than 0 and up to 100 for MARKET orders")
	}
	return nil
}

func isZeroDecimal(value string) bool {
	if value == "" {
		return true
	}
	if !decimalPattern.MatchString(value) {
		return false
	}
	parsed, ok := new(big.Rat).SetString(value)
	return ok && parsed.Sign() == 0
}

func ValidateExecutionDate(value string, now time.Time, location *time.Location) error {
	date, err := time.ParseInLocation("2006-01-02", value, location)
	if err != nil {
		return errors.New("execution date must be YYYY-MM-DD")
	}
	today := time.Date(now.In(location).Year(), now.In(location).Month(), now.In(location).Day(), 0, 0, 0, 0, location)
	if date.Before(today) {
		return errors.New("execution date is in the past")
	}
	if date.Weekday() == time.Saturday || date.Weekday() == time.Sunday {
		return errors.New("execution date cannot be a weekend")
	}
	return nil
}

func positiveDecimal(name, value string) error {
	if err := optionalDecimal(name, value); err != nil {
		return err
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed <= 0 {
		return fmt.Errorf("%s must be a positive decimal", name)
	}
	return nil
}

func optionalDecimal(name, value string) error {
	if value == "" {
		return nil
	}
	if !decimalPattern.MatchString(value) {
		return fmt.Errorf("%s must be a plain non-negative decimal", name)
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return fmt.Errorf("%s cannot be represented as a finite decimal", name)
	}
	input, inputOK := new(big.Rat).SetString(value)
	roundTrip, roundTripOK := new(big.Rat).SetString(strconv.FormatFloat(parsed, 'f', -1, 64))
	if !inputOK || !roundTripOK || input.Cmp(roundTrip) != 0 {
		return fmt.Errorf("%s cannot be represented without changing its decimal value", name)
	}
	return nil
}

func oneOf(value string, choices ...string) bool {
	for _, choice := range choices {
		if value == choice {
			return true
		}
	}
	return false
}
