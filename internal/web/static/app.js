(() => {
  "use strict";

  const reducedMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  const toastRegion = document.querySelector("[data-toast-region]");
  const pageClock = document.querySelector("[data-ist-clock]");
  const serverEpochAtScriptStart = Number(pageClock?.dataset.serverEpochMs);
  const scriptMonotonicStart = performance.now();
  const serverNow = () => Number.isFinite(serverEpochAtScriptStart)
    ? serverEpochAtScriptStart + (performance.now() - scriptMonotonicStart)
    : Date.now();

  function toast(message, isError = false) {
    if (!toastRegion) return;
    const node = document.createElement("div");
    node.className = `toast${isError ? " is-error" : ""}`;
    node.textContent = message;
    toastRegion.append(node);
    window.setTimeout(() => node.remove(), isError ? 6500 : 3500);
  }

  function pad(value, width = 2) {
    return String(value).padStart(width, "0");
  }

  /* Server-anchored IST clock. The ticking text is intentionally not a live region. */
  document.querySelectorAll("[data-ist-clock]").forEach((clock) => {
    const serverEpoch = Number(clock.dataset.serverEpochMs) || Date.now();
    const monotonicStart = performance.now();
    const dateNode = clock.querySelector("[data-clock-date]");
    const timeNode = clock.querySelector("[data-clock-time]");
    const monthNames = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
    let lastDate = "";
    let lastSecond = -1;

    const render = () => {
      // IST has no daylight-saving transition, so a fixed UTC offset is safe.
      const epoch = serverEpoch + (performance.now() - monotonicStart);
      const india = new Date(epoch + 19_800_000);
      const second = india.getUTCSeconds();
      const milliseconds = india.getUTCMilliseconds();
      timeNode.textContent = `${pad(india.getUTCHours())}:${pad(india.getUTCMinutes())}:${pad(second)}.${pad(milliseconds, 3)}`;
      if (second !== lastSecond) {
        const nextDate = `${pad(india.getUTCDate())} ${monthNames[india.getUTCMonth()]} ${india.getUTCFullYear()}`;
        if (nextDate !== lastDate) {
          dateNode.textContent = nextDate;
          lastDate = nextDate;
        }
        clock.dateTime = new Date(epoch).toISOString();
        lastSecond = second;
      }
      if (!document.hidden) window.requestAnimationFrame(render);
    };

    render();
    document.addEventListener("visibilitychange", () => {
      if (!document.hidden) render();
    });
  });

  /* Refresh stateful pages just after their next server-provided boundary. */
  const maximumBoundaryDelay = 36 * 60 * 60 * 1000;
  const parseRefreshAt = (value) => {
    const trimmed = String(value || "").trim();
    if (!trimmed) return NaN;
    const numeric = Number(trimmed);
    return Number.isFinite(numeric) ? numeric : Date.parse(trimmed);
  };
  const currentEpoch = serverNow();
  const nextRefreshAt = [...document.querySelectorAll("[data-refresh-at]")]
    .map((node) => parseRefreshAt(node.dataset.refreshAt))
    .filter((epoch) => Number.isFinite(epoch) && epoch > currentEpoch && epoch - currentEpoch <= maximumBoundaryDelay)
    .sort((left, right) => left - right)[0];
  if (nextRefreshAt) {
    let refreshTimer = 0;
    let refreshing = false;
    const refreshAtBoundary = () => {
      if (refreshing) return;
      const remaining = nextRefreshAt - serverNow();
      if (remaining > 0) {
        window.clearTimeout(refreshTimer);
        refreshTimer = window.setTimeout(refreshAtBoundary, remaining + 250);
        return;
      }
      refreshing = true;
      window.location.reload();
    };
    refreshTimer = window.setTimeout(refreshAtBoundary, nextRefreshAt - currentEpoch + 250);
    document.addEventListener("visibilitychange", () => {
      if (!document.hidden && serverNow() >= nextRefreshAt) refreshAtBoundary();
    });
  }

  /* Navigation */
  const layout = document.querySelector("[data-app-layout]");
  const sideRail = layout?.querySelector("[data-side-rail]");
  const workspace = layout?.querySelector(".workspace");
  const mobileNav = layout?.querySelector(".mobile-nav");
  const mobileNavigation = window.matchMedia("(max-width: 960px)");
  const navOpeners = document.querySelectorAll("[data-nav-open]");
  let navTrigger = null;
  function navigationFocusables() {
    if (!sideRail) return [];
    return [...sideRail.querySelectorAll('a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])')]
      .filter((node) => !node.hidden && node.getClientRects().length > 0);
  }
  function syncNavigationState() {
    if (!layout || !sideRail) return;
    const mobile = mobileNavigation.matches;
    const open = mobile && layout.classList.contains("nav-open");
    sideRail.inert = mobile && !open;
    workspace && (workspace.inert = open);
    mobileNav && (mobileNav.inert = open);
    document.body.classList.toggle("nav-open-lock", open);
    if (open) {
      sideRail.setAttribute("role", "dialog");
      sideRail.setAttribute("aria-modal", "true");
      sideRail.setAttribute("aria-label", "Application navigation");
    } else {
      sideRail.removeAttribute("role");
      sideRail.removeAttribute("aria-modal");
      sideRail.removeAttribute("aria-label");
    }
  }
  function closeNavigation() {
    if (!layout) return;
    layout.classList.remove("nav-open");
    navOpeners.forEach((button) => button.setAttribute("aria-expanded", "false"));
    syncNavigationState();
    if (navTrigger) navTrigger.focus();
  }
  navOpeners.forEach((button) => button.addEventListener("click", () => {
    if (!layout) return;
    navTrigger = button;
    layout.classList.add("nav-open");
    button.setAttribute("aria-expanded", "true");
    syncNavigationState();
    layout.querySelector("[data-nav-close]")?.focus();
  }));
  document.querySelectorAll("[data-nav-close]").forEach((button) => button.addEventListener("click", closeNavigation));
  document.addEventListener("keydown", (event) => {
    if (event.key === "Escape" && layout?.classList.contains("nav-open")) closeNavigation();
    if (event.key === "Tab" && layout?.classList.contains("nav-open") && mobileNavigation.matches) {
      const focusable = navigationFocusables();
      if (!focusable.length) return;
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus(); }
      else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
    }
  });
  document.querySelectorAll("[data-nav-link]").forEach((link) => {
    const linkPath = new URL(link.href, window.location.origin).pathname.replace(/\/$/, "") || "/";
    const currentPath = window.location.pathname.replace(/\/$/, "") || "/";
    const exact = link.hasAttribute("data-nav-exact");
    if ((exact && currentPath === linkPath) || (!exact && (currentPath === linkPath || currentPath.startsWith(`${linkPath}/`)))) {
      link.setAttribute("aria-current", "page");
    }
  });
  document.querySelectorAll("[data-nav-more]").forEach((button) => {
    const currentPath = window.location.pathname.replace(/\/$/, "") || "/";
    const paths = button.dataset.navMore.split(",").map((path) => path.trim()).filter(Boolean);
    if (paths.some((path) => currentPath === path || currentPath.startsWith(`${path}/`))) button.setAttribute("aria-current", "page");
  });
  mobileNavigation.addEventListener("change", () => {
    if (!mobileNavigation.matches) layout?.classList.remove("nav-open");
    syncNavigationState();
  });
  syncNavigationState();

  /* Password visibility */
  document.querySelectorAll("[data-password-toggle]").forEach((button) => {
    button.addEventListener("click", () => {
      const field = document.getElementById(button.dataset.passwordToggle);
      if (!field) return;
      const showing = field.type === "text";
      field.type = showing ? "password" : "text";
      button.textContent = showing ? "Show" : "Hide";
      button.setAttribute("aria-pressed", String(!showing));
      field.focus({ preventScroll: true });
    });
  });

  /* Copy deployment values without injecting HTML. */
  document.querySelectorAll("[data-copy-target]").forEach((button) => {
    button.addEventListener("click", async () => {
      const source = document.getElementById(button.dataset.copyTarget);
      const value = source?.textContent?.trim();
      if (!value) return;
      try {
        await navigator.clipboard.writeText(value);
        toast("Copied to clipboard.");
      } catch (_) {
        const input = document.createElement("textarea");
        input.value = value;
        input.setAttribute("readonly", "");
        input.className = "visually-hidden";
        document.body.append(input);
        input.select();
        const copied = document.execCommand("copy");
        input.remove();
        toast(copied ? "Copied to clipboard." : "Could not copy. Select the value manually.", !copied);
      }
    });
  });

  /* LIMIT/MARKET progressive disclosure. */
  document.querySelectorAll("[data-order-form]").forEach((form) => {
    const priceWrap = form.querySelector("[data-limit-price-field]");
    const price = form.querySelector("[data-limit-price]");
    const types = [...form.querySelectorAll("[data-order-type]")];
    const reflectValidity = (control, invalid) => {
      if (!(control instanceof HTMLInputElement || control instanceof HTMLSelectElement)) return;
      if (invalid) control.setAttribute("aria-invalid", "true");
      else control.removeAttribute("aria-invalid");
      control.closest(".money-input")?.classList.toggle("is-invalid", invalid);
    };
    const update = () => {
      const limit = types.find((input) => input.checked)?.value === "LIMIT";
      if (priceWrap) priceWrap.hidden = !limit;
      if (price) {
        price.disabled = !limit;
        price.required = limit;
        if (!limit) {
          price.setCustomValidity("");
          reflectValidity(price, false);
        }
      }
    };
    types.forEach((input) => input.addEventListener("change", update));
    form.addEventListener("invalid", (event) => reflectValidity(event.target, true), true);
    ["input", "change"].forEach((eventName) => form.addEventListener(eventName, (event) => {
      const control = event.target;
      if ((control instanceof HTMLInputElement || control instanceof HTMLSelectElement) && control.validity.valid) {
        reflectValidity(control, false);
      }
    }));
    update();
  });

  /* Keep a date-dependent dispatch preview truthful when the date changes. */
  document.querySelectorAll("[data-schedule-preview-url]").forEach((dateInput) => {
    const form = dateInput.closest("form");
    const schedule = form?.querySelector("[data-dispatch-schedule]");
    const dispatchHint = form?.querySelector("[data-dispatch-hint]");
    const freezeHint = form?.querySelector("[data-schedule-freeze-hint]");
    if (!form || !schedule) return;
    let controller = null;
    let generation = 0;

    const setPreviewState = (state, value, message) => {
      schedule.value = value;
      schedule.dataset.previewState = state;
      if (message && dispatchHint) dispatchHint.textContent = message;
    };

    const updatePreview = async () => {
      generation += 1;
      const currentGeneration = generation;
      controller?.abort();
      controller = null;
      const executionDate = dateInput.value.trim();
      if (!executionDate || !dateInput.validity.valid) return;

      const currentController = new AbortController();
      controller = currentController;
      schedule.setAttribute("aria-busy", "true");
      setPreviewState("loading", "Updating schedule…", "Checking the dispatch schedule for the selected date…");
      try {
        const url = new URL(dateInput.dataset.schedulePreviewUrl, window.location.origin);
        url.searchParams.set("date", executionDate);
        const response = await fetch(url, { headers: { Accept: "application/json" }, cache: "no-store", signal: currentController.signal });
        if (!response.ok) throw new Error(`schedule preview failed (${response.status})`);
        const payload = await response.json();
        if (currentGeneration !== generation) return;
        if (!payload.dispatch_time || !payload.freeze_time) throw new Error("schedule preview is incomplete");
        setPreviewState("ready", `From ${payload.dispatch_time} IST`, "Your precise slot is shown after the order joins the queue.");
        if (freezeHint) freezeHint.textContent = `Same-day creation closes at ${payload.freeze_time} IST. Trading holidays are not inferred.`;
      } catch (errorValue) {
        if (errorValue.name !== "AbortError" && currentGeneration === generation) {
          setPreviewState("error", "Schedule unavailable", "The preview could not be refreshed. The selected date will still be validated when you submit.");
        }
      } finally {
        if (currentGeneration === generation) {
          controller = null;
          schedule.removeAttribute("aria-busy");
        }
      }
    };

    dateInput.addEventListener("change", updatePreview);
  });

  /* Accessible async symbol combobox. */
  document.querySelectorAll("[data-symbol-combobox]").forEach((root) => {
    const input = root.querySelector("[data-symbol-query]");
    const hidden = root.querySelector("[data-symbol-value]");
    const results = root.querySelector("[data-symbol-results]");
    const spinner = root.querySelector("[data-symbol-spinner]");
    const field = root.closest(".field");
    const error = field?.querySelector("[data-symbol-error]");
    const hint = field?.querySelector("[data-symbol-hint]");
    const exchange = root.closest("form")?.querySelector("[data-symbol-exchange]");
    const searchURL = root.dataset.searchUrl;
    if (!input || !hidden || !results) return;
    const emptyHint = root.dataset.emptyHint || "Select a result; free-form symbols are not accepted.";
    let options = [];
    let activeIndex = -1;
    let timer = 0;
    let controller = null;
    let requestGeneration = 0;

    function setHint(message) {
      if (hint) hint.textContent = message;
    }

    function setBusy(busy) {
      if (spinner) spinner.hidden = !busy;
      if (busy) root.setAttribute("aria-busy", "true");
      else root.removeAttribute("aria-busy");
    }

    function cancelActiveRequest() {
      requestGeneration += 1;
      controller?.abort();
      controller = null;
      setBusy(false);
    }

    function cancelSearch() {
      window.clearTimeout(timer);
      timer = 0;
      cancelActiveRequest();
    }

    function close() {
      results.hidden = true;
      input.setAttribute("aria-expanded", "false");
      input.removeAttribute("aria-activedescendant");
      activeIndex = -1;
    }

    function clearResults() {
      results.replaceChildren();
      options = [];
      close();
    }

    function highlight(index) {
      if (!options.length) return;
      activeIndex = (index + options.length) % options.length;
      options.forEach((option, optionIndex) => option.setAttribute("aria-selected", String(optionIndex === activeIndex)));
      const active = options[activeIndex];
      input.setAttribute("aria-activedescendant", active.id);
      active.scrollIntoView({ block: "nearest" });
    }

    function select(option) {
      cancelSearch();
      input.value = option.dataset.symbol;
      hidden.value = option.dataset.symbol;
      input.setCustomValidity("");
      input.removeAttribute("aria-invalid");
      if (error) error.hidden = true;
      setHint(`${option.dataset.exchange} · ${option.dataset.name || option.dataset.symbol} selected.`);
      close();
      input.focus();
    }

    function renderMessage(message, isError = false) {
      results.replaceChildren();
      const messageNode = document.createElement("div");
      messageNode.className = `combobox-message${isError ? " is-error" : ""}`;
      messageNode.textContent = message;
      results.append(messageNode);
      options = [];
      results.hidden = false;
      input.setAttribute("aria-expanded", "true");
      input.removeAttribute("aria-activedescendant");
      activeIndex = -1;
    }

    function render(items) {
      results.replaceChildren();
      if (!items.length) {
        renderMessage("No matching equity symbols.");
        setHint("No matching equity symbols. Try a different symbol or company name.");
        return;
      }

      items.slice(0, 40).forEach((item, index) => {
        const symbol = item.trading_symbol || item.symbol || item.TradingSymbol || "";
        const itemExchange = item.exchange || item.Exchange || exchange?.value || "";
        const name = item.name || item.Name || "";
        const option = document.createElement("button");
        option.type = "button";
        option.className = "combobox-option";
        option.id = `${results.id}-option-${index}`;
        option.setAttribute("role", "option");
        option.setAttribute("aria-selected", "false");
        option.dataset.symbol = symbol;
        option.dataset.exchange = itemExchange;
        option.dataset.name = name;
        const primary = document.createElement("span");
        const strong = document.createElement("strong");
        const small = document.createElement("small");
        strong.textContent = symbol;
        small.textContent = name || "Equity instrument";
        primary.append(strong, small);
        const market = document.createElement("span");
        market.className = "exchange-tag";
        market.textContent = itemExchange;
        option.append(primary, market);
        option.addEventListener("pointerdown", (event) => event.preventDefault());
        option.addEventListener("click", () => select(option));
        results.append(option);
      });
      options = [...results.querySelectorAll("[role=option]")];
      results.hidden = false;
      input.setAttribute("aria-expanded", "true");
      setHint(`${options.length} matching symbol${options.length === 1 ? "" : "s"}. Use arrow keys to review.`);
    }

    async function search() {
      window.clearTimeout(timer);
      timer = 0;
      cancelActiveRequest();
      const query = input.value.trim();
      if (query.length < 1 || !searchURL) {
        close();
        setHint(emptyHint);
        return;
      }
      const generation = requestGeneration;
      const currentController = new AbortController();
      controller = currentController;
      setBusy(true);
      try {
        const url = new URL(searchURL, window.location.origin);
        url.searchParams.set("q", query);
        if (exchange?.value) url.searchParams.set("exchange", exchange.value);
        const response = await fetch(url, { headers: { Accept: "application/json" }, cache: "no-store", signal: currentController.signal });
        if (!response.ok) throw new Error(`search failed (${response.status})`);
        const payload = await response.json();
        if (generation !== requestGeneration) return;
        render(Array.isArray(payload) ? payload : (payload.instruments || payload.results || []));
      } catch (errorValue) {
        if (errorValue.name !== "AbortError" && generation === requestGeneration) {
          const message = "Symbol search is temporarily unavailable. Try again.";
          renderMessage(message, true);
          setHint(message);
        }
      } finally {
        if (generation === requestGeneration) {
          controller = null;
          setBusy(false);
        }
      }
    }

    input.addEventListener("input", () => {
      cancelSearch();
      close();
      hidden.value = "";
      input.setCustomValidity("");
      input.removeAttribute("aria-invalid");
      if (error) error.hidden = true;
      if (input.value.trim()) {
        setHint("Searching known equity symbols…");
        timer = window.setTimeout(search, 160);
      } else {
        setHint(emptyHint);
      }
    });
    input.addEventListener("keydown", (event) => {
      if (event.key === "ArrowDown") { event.preventDefault(); if (results.hidden || !options.length) search(); else highlight(activeIndex + 1); }
      else if (event.key === "ArrowUp") { event.preventDefault(); highlight(activeIndex - 1); }
      else if (event.key === "Enter" && activeIndex >= 0) { event.preventDefault(); select(options[activeIndex]); }
      else if (event.key === "Escape") { event.preventDefault(); cancelSearch(); close(); }
      else if (event.key === "Tab") { cancelSearch(); close(); }
    });
    input.addEventListener("blur", () => window.setTimeout(() => {
      if (!root.contains(document.activeElement)) {
        cancelSearch();
        close();
      }
    }, 120));
    exchange?.addEventListener("change", () => {
      cancelSearch();
      clearResults();
      hidden.value = "";
      input.value = "";
      input.setCustomValidity("");
      input.removeAttribute("aria-invalid");
      if (error) error.hidden = true;
      setHint(emptyHint);
    });
    root.closest("form")?.addEventListener("submit", (event) => {
      if (!hidden.value) {
        cancelSearch();
        event.preventDefault();
        input.setAttribute("aria-invalid", "true");
        input.setCustomValidity("Select a symbol from the search results.");
        if (error) error.hidden = false;
        input.reportValidity();
        input.focus();
      } else input.setCustomValidity("");
    });
  });

  /* Atomic queue reordering, with pointer drag and explicit keyboard-accessible buttons. */
  document.querySelectorAll("[data-reorder-list]").forEach((list) => {
    const batchSize = Number(list.dataset.batchSize) || 10;
    const batchGap = Number(list.dataset.batchGapMs) || 1050;
    const baseTime = list.dataset.dispatchBase || "00:00:00";
    const status = list.querySelector("[data-reorder-status]");
    let dragItem = null;
    let dragSnapshot = [];
    let saving = false;

    const items = () => [...list.querySelectorAll(":scope > [data-reorder-item]")];
    const parseBase = () => {
      const parts = baseTime.split(":");
      const secondsParts = (parts[2] || "0").split(".");
      return ((Number(parts[0]) * 60 + Number(parts[1])) * 60 + Number(secondsParts[0])) * 1000 + Number((secondsParts[1] || "0").padEnd(3, "0").slice(0, 3));
    };
    const formatClock = (total) => {
      const day = 86_400_000;
      total = ((total % day) + day) % day;
      const hours = Math.floor(total / 3_600_000);
      const minutes = Math.floor((total % 3_600_000) / 60_000);
      const seconds = Math.floor((total % 60_000) / 1000);
      return `${pad(hours)}:${pad(minutes)}:${pad(seconds)}.${pad(total % 1000, 3)}`;
    };

    function rebuild() {
      list.querySelectorAll(":scope > [data-batch-divider]").forEach((divider) => divider.remove());
      const all = items();
      all.forEach((item, index) => {
        const position = index + 1;
        const batch = Math.floor(index / batchSize) + 1;
        const planned = formatClock(parseBase() + (batch - 1) * batchGap);
        if (index % batchSize === 0) {
          const divider = document.createElement("div");
          divider.className = "batch-divider";
          divider.dataset.batchDivider = "";
          const label = document.createElement("span");
          const time = document.createElement("time");
          label.textContent = `Batch ${batch}`;
          time.dataset.batchTime = "";
          time.textContent = `${planned} IST`;
          divider.append(label, time);
          list.insertBefore(divider, item);
        }
        const positionNode = item.querySelector("[data-position]");
        if (positionNode) { positionNode.textContent = String(position); positionNode.setAttribute("aria-label", `Queue position ${position}`); }
        item.querySelectorAll("[data-planned-time]").forEach((node) => { node.textContent = `${planned} IST`; });
        item.querySelectorAll("[data-batch-number]").forEach((node) => { node.textContent = String(batch); });
        const up = item.querySelector("[data-move=up]");
        const down = item.querySelector("[data-move=down]");
        if (up) up.disabled = index === 0;
        if (down) down.disabled = index === all.length - 1;
      });
    }

    function restore(order) {
      const byID = new Map(items().map((item) => [item.dataset.orderId, item]));
      order.forEach((id) => { const item = byID.get(id); if (item) list.append(item); });
      rebuild();
    }

    async function persist(previousOrder, movedItem) {
      if (saving) return;
      saving = true;
      const all = items();
      list.classList.add("is-saving");
      list.setAttribute("aria-busy", "true");
      list.inert = true;
      const parameters = new URLSearchParams();
      parameters.set("csrf", list.querySelector("[data-reorder-csrf]")?.value || document.querySelector('meta[name="csrf-token"]')?.content || "");
      parameters.set("queue_revision", list.dataset.queueRevision || "0");
      if (list.dataset.traderId) parameters.set("trader_id", list.dataset.traderId);
      if (list.dataset.executionDate) parameters.set("execution_date", list.dataset.executionDate);
      all.forEach((item) => parameters.append("order_ids", item.dataset.orderId));
      try {
        const response = await fetch(list.dataset.reorderUrl, { method: "POST", headers: { "Content-Type": "application/x-www-form-urlencoded", Accept: "application/json" }, body: parameters.toString(), credentials: "same-origin" });
        if (!response.ok) throw new Error(`save failed (${response.status})`);
        const payload = response.headers.get("content-type")?.includes("application/json") ? await response.json() : {};
        if (payload.revision != null) list.dataset.queueRevision = String(payload.revision);
        else list.dataset.queueRevision = String((Number(list.dataset.queueRevision) || 0) + 1);
        const ordered = items();
        const index = ordered.indexOf(movedItem);
        const batch = Math.floor(index / batchSize) + 1;
        const planned = movedItem.querySelector("[data-planned-time]")?.textContent || "its displayed time";
        const message = `${movedItem.dataset.symbol || "Order"} moved to position ${index + 1}, batch ${batch}, ${planned}.`;
        if (status) status.textContent = message;
        toast("Queue order saved.");
      } catch (_) {
        restore(previousOrder);
        if (status) status.textContent = "The queue could not be confirmed. Reloading the saved server order.";
        toast("Could not confirm the queue. Reloading server state.", true);
        window.setTimeout(() => window.location.reload(), 50);
      } finally {
        saving = false;
        list.inert = false;
        list.classList.remove("is-saving");
        list.removeAttribute("aria-busy");
      }
    }

    list.addEventListener("click", (event) => {
      const button = event.target.closest("[data-move]");
      if (saving || !button || button.disabled) return;
      const item = button.closest("[data-reorder-item]");
      const before = items().map((node) => node.dataset.orderId);
      const all = items();
      const index = all.indexOf(item);
      if (button.dataset.move === "up" && index > 0) list.insertBefore(item, all[index - 1]);
      else if (button.dataset.move === "down" && index < all.length - 1) list.insertBefore(all[index + 1], item);
      else return;
      rebuild();
      item.querySelector(`[data-move="${button.dataset.move}"]`)?.focus();
      persist(before, item);
    });
    list.addEventListener("dragstart", (event) => {
      if (saving) { event.preventDefault(); return; }
      dragItem = event.target.closest("[data-reorder-item]");
      if (!dragItem) return;
      dragSnapshot = items().map((item) => item.dataset.orderId);
      dragItem.classList.add("is-dragging");
      event.dataTransfer.effectAllowed = "move";
      event.dataTransfer.setData("text/plain", dragItem.dataset.orderId);
    });
    list.addEventListener("dragover", (event) => {
      if (saving || !dragItem) return;
      event.preventDefault();
      const target = event.target.closest("[data-reorder-item]");
      if (!target || target === dragItem) return;
      list.querySelectorAll(".is-drag-target").forEach((item) => item.classList.remove("is-drag-target"));
      target.classList.add("is-drag-target");
      const before = event.clientY < target.getBoundingClientRect().top + target.offsetHeight / 2;
      list.insertBefore(dragItem, before ? target : target.nextSibling);
      rebuild();
    });
    list.addEventListener("drop", (event) => {
      if (!dragItem) return;
      event.preventDefault();
      const moved = dragItem;
      list.querySelectorAll(".is-drag-target").forEach((item) => item.classList.remove("is-drag-target"));
      moved.classList.remove("is-dragging");
      dragItem = null;
      persist(dragSnapshot, moved);
    });
    list.addEventListener("dragend", () => {
      if (dragItem && dragSnapshot.length) restore(dragSnapshot);
      list.querySelectorAll(".is-dragging,.is-drag-target").forEach((item) => item.classList.remove("is-dragging", "is-drag-target"));
      dragItem = null;
      dragSnapshot = [];
    });
    rebuild();
  });

  /* Dialogs and typed destructive confirmations. */
  document.querySelectorAll("[data-dialog-open]").forEach((button) => button.addEventListener("click", () => {
    const dialog = document.getElementById(button.dataset.dialogOpen);
    dialog?.showModal();
    dialog?.querySelector("input:not([type=hidden])")?.focus();
  }));
  document.querySelectorAll("[data-dialog-close]").forEach((button) => button.addEventListener("click", () => button.closest("dialog")?.close()));
  document.querySelectorAll("[data-user-delete]").forEach((button) => button.addEventListener("click", () => {
    const dialog = document.getElementById("delete-user-dialog");
    const form = dialog?.querySelector("[data-user-delete-form]");
    const label = dialog?.querySelector("[data-delete-client-label]");
    const input = dialog?.querySelector("[data-confirm-input]");
    if (!dialog || !form || !input) return;
    form.action = button.dataset.deleteAction;
    form.dataset.confirmValue = button.dataset.clientId;
    if (label) label.textContent = button.dataset.clientId;
    input.value = "";
    input.dispatchEvent(new Event("input"));
    dialog.showModal();
    input.focus();
  }));
  document.querySelectorAll("[data-destructive-form]").forEach((form) => {
    const input = form.querySelector("[data-confirm-input]");
    const submit = form.querySelector("[data-destructive-submit]");
    if (!input || !submit) return;
    const update = () => { submit.disabled = input.value.trim().toUpperCase() !== (form.dataset.confirmValue || "").trim().toUpperCase(); };
    input.addEventListener("input", update);
    update();
  });
  document.querySelectorAll("form[data-confirm]").forEach((form) => form.addEventListener("submit", (event) => {
    if (!window.confirm(form.dataset.confirm)) event.preventDefault();
  }));

  /* Admin password confirmation. */
  document.querySelectorAll("[data-password-form]").forEach((form) => {
    const password = form.querySelector("[data-new-password]");
    const confirmation = form.querySelector("[data-confirm-password]");
    const error = form.querySelector("[data-password-error]");
    const validate = () => {
      const mismatch = confirmation.value && password.value !== confirmation.value;
      confirmation.setCustomValidity(mismatch ? "The passwords do not match." : "");
      if (error) error.hidden = !mismatch;
    };
    password?.addEventListener("input", validate);
    confirmation?.addEventListener("input", validate);
  });

  /* Settings preview. */
  const settingsForm = document.querySelector(".settings-form");
  if (settingsForm) {
    const dispatch = settingsForm.querySelector('[name="dispatch_clock"]');
    const lead = settingsForm.querySelector('[name="freeze_lead_minutes"]');
    const blackoutStart = settingsForm.querySelector('[name="blackout_start"]');
    const blackoutEnd = settingsForm.querySelector('[name="blackout_end"]');
    const dispatchPreview = settingsForm.querySelector("[data-settings-dispatch-preview]");
    const freezePreview = settingsForm.querySelector("[data-settings-freeze-preview]");
    const blackoutStartPreview = settingsForm.querySelector("[data-settings-blackout-start-preview]");
    const blackoutEndPreview = settingsForm.querySelector("[data-settings-blackout-end-preview]");
    const update = () => {
      const parts = (dispatch.value || "00:00:00").split(":").map(Number);
      const total = ((parts[0] || 0) * 60 + (parts[1] || 0)) * 60 + (parts[2] || 0) - (Number(lead.value) || 0) * 60;
      const normalized = ((total % 86400) + 86400) % 86400;
      if (dispatchPreview) dispatchPreview.textContent = `${dispatch.value || "—"} IST`;
      if (freezePreview) freezePreview.textContent = `${pad(Math.floor(normalized / 3600))}:${pad(Math.floor((normalized % 3600) / 60))}:${pad(normalized % 60)} IST`;
      if (blackoutStartPreview) blackoutStartPreview.textContent = blackoutStart.value || "—";
      if (blackoutEndPreview) blackoutEndPreview.textContent = blackoutEnd.value || "—";
    };
    dispatch?.addEventListener("input", update);
    lead?.addEventListener("input", update);
    blackoutStart?.addEventListener("input", update);
    blackoutEnd?.addEventListener("input", update);
    update();
  }

  /* Prevent accidental duplicate form submission after native validation succeeds. */
  document.querySelectorAll("form[data-disable-on-submit]").forEach((form) => form.addEventListener("submit", () => {
    if (!form.checkValidity()) return;
    window.requestAnimationFrame(() => form.querySelectorAll('button[type="submit"]').forEach((button) => {
      button.disabled = true;
      button.setAttribute("aria-busy", "true");
    }));
  }));
})();
