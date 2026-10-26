/*
 * KioskApi — the only place the kiosk page talks to the outside world.
 *
 * Real mode (the default): every call goes to the Kiosk Agent that serves this page
 * (apiBase, default /api). The agent adds the machine key, forwards to the server,
 * drives the motor board and journals each vend, so the page itself stores nothing durable.
 *
 * Mock mode: the same functions run against an in-memory store, so the UI can be
 * developed, tested and demoed without the agent, the server or the hardware.
 *
 * Errors are thrown as ApiError with a `code`:
 *   OFFLINE         server/agent not reachable, or the server refuses this machine
 *                   (data.reason INVALID_MACHINE_KEY or MACHINE_INACTIVE, with data.message)
 *   CATALOG_STALE   purchase made against an old catalog version (data.catalog = fresh catalog)
 *   SOLD_OUT        slot no longer available
 *   EXPIRED         payment window (60 s) passed
 *   INVALID_LOGIN   wrong username or password (data.attemptsLeft)
 *   LOCKED          too many wrong attempts (data.retryInSeconds)
 *   INVALID_INPUT   a restock value outside 0..capacity
 *   UNAUTHORIZED    operator session no longer valid
 */
(function (global) {
  'use strict';

  var cfg = Object.assign({ mock: true, apiBase: '/api', timeoutMs: 10000 }, global.KIOSK_CONFIG || {});
  var params = new URLSearchParams(global.location.search);
  if (params.has('mock')) cfg.mock = params.get('mock') !== '0';
  var sim = params.get('sim') || cfg.sim || 'ok'; // mock only: ok | error | offline

  function ApiError(code, message, data) {
    var e = new Error(message || code);
    e.name = 'ApiError';
    e.code = code;
    e.data = data || {};
    return e;
  }

  function uuid() {
    if (global.crypto && global.crypto.randomUUID) return global.crypto.randomUUID();
    var b = new Uint8Array(16);
    global.crypto.getRandomValues(b);
    b[6] = (b[6] & 0x0f) | 0x40;
    b[8] = (b[8] & 0x3f) | 0x80;
    var h = Array.prototype.map.call(b, function (x) { return (x + 256).toString(16).slice(1); }).join('');
    return h.slice(0, 8) + '-' + h.slice(8, 12) + '-' + h.slice(12, 16) + '-' + h.slice(16, 20) + '-' + h.slice(20);
  }

  function sleep(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }

  /* ---------------------------------------------------------------- real */

  async function http(method, path, body, token, timeoutMs) {
    var ctrl = new AbortController();
    var timer = setTimeout(function () { ctrl.abort(); }, timeoutMs || cfg.timeoutMs);
    var res;
    try {
      var headers = { 'Content-Type': 'application/json' };
      if (token) headers.Authorization = 'Bearer ' + token;
      res = await fetch(cfg.apiBase + path, {
        method: method,
        headers: headers,
        body: body === undefined ? undefined : JSON.stringify(body),
        signal: ctrl.signal,
        cache: 'no-store'
      });
    } catch (err) {
      throw ApiError('OFFLINE', 'Server tidak terjangkau');
    } finally {
      clearTimeout(timer);
    }
    var data = null;
    try { data = await res.json(); } catch (_) { /* empty body */ }
    if (!res.ok) {
      throw ApiError((data && data.code) || 'HTTP_' + res.status, data && data.message, data);
    }
    return data;
  }

  // The agent answers "pay" only after the coil has turned (up to 5 s) and the
  // server has the result, so that call gets a longer wait than the others.
  var PAY_TIMEOUT_MS = 30000;
  var RESTOCK_TIMEOUT_MS = 20000;

  var realApi = {
    health: function () { return http('GET', '/health'); },
    getCatalog: function () { return http('GET', '/catalog'); },
    createTransaction: function (p) { return http('POST', '/transactions', p); },
    pay: function (id) { return http('POST', '/transactions/' + encodeURIComponent(id) + '/pay', {}, null, PAY_TIMEOUT_MS); },
    cancel: function (id, reason) { return http('POST', '/transactions/' + encodeURIComponent(id) + '/cancel', { reason: reason }); },
    report: function (id) { return http('POST', '/transactions/' + encodeURIComponent(id) + '/report', {}); },
    login: function (username, password) { return http('POST', '/operator/login', { username: username, password: password }); },
    logout: function (token) { return http('POST', '/operator/logout', {}, token); },
    getRestockSlots: function (token) { return http('GET', '/operator/slots', undefined, token); },
    saveRestock: function (token, session) { return http('POST', '/restock-sessions', session, token, RESTOCK_TIMEOUT_MS); }
  };

  /* ---------------------------------------------------------------- mock */

  var PALETTE = ['#7b3fa0', '#1f7a8c', '#c0392b', '#2e8b57', '#d35400', '#34495e', '#8e44ad'];

  function placeholderImage(name, i) {
    var color = PALETTE[i % PALETTE.length];
    var initials = name.split(/\s+/).map(function (w) { return w[0]; }).join('').slice(0, 2).toUpperCase();
    var svg =
      '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 300 400">' +
      '<rect width="300" height="400" rx="18" fill="' + color + '"/>' +
      '<rect x="18" y="18" width="264" height="364" rx="12" fill="none" stroke="#fff" stroke-opacity=".35" stroke-width="4"/>' +
      '<circle cx="150" cy="170" r="74" fill="#fff" fill-opacity=".16"/>' +
      '<text x="150" y="196" font-family="Segoe UI, Arial, sans-serif" font-size="76" font-weight="700" fill="#fff" text-anchor="middle">' + initials + '</text>' +
      '<text x="150" y="320" font-family="Segoe UI, Arial, sans-serif" font-size="30" font-weight="600" fill="#fff" fill-opacity=".9" text-anchor="middle">GACHA</text>' +
      '</svg>';
    return 'data:image/svg+xml;charset=utf-8,' + encodeURIComponent(svg);
  }

  function product(id, name, description, price) {
    return { id: id, name: name, description: description, price: price, imageUrl: placeholderImage(name, id), isActive: true };
  }

  var P = [
    product(1, 'Gacha Legendary', 'Satu pack berisi 5 kartu, peluang kartu legendaris.', 50000),
    product(2, 'Gacha Ultra Rare', 'Satu pack berisi 5 kartu, minimal 1 ultra rare.', 35000),
    product(3, 'Gacha Rare', 'Satu pack berisi 5 kartu, minimal 1 rare.', 25000),
    product(4, 'Gacha Uncommon', 'Satu pack berisi 5 kartu uncommon.', 15000),
    product(5, 'Gacha Common', 'Satu pack berisi 5 kartu common.', 10000),
    product(6, 'Gacha Mystery Box', 'Isi acak dari semua tingkat.', 30000),
    product(7, 'Gacha Promo Edition', 'Edisi promo terbatas.', 40000)
  ];

  var store = {
    catalogVersion: 1,
    machine: { code: 'VM-01', name: 'Vending Machine Lobby' },
    slots: [
      { slotNumber: 1, capacity: 10, qty: 3, needsCheck: false, product: P[0], nextProduct: null },
      { slotNumber: 2, capacity: 10, qty: 6, needsCheck: false, product: P[1], nextProduct: null },
      { slotNumber: 3, capacity: 10, qty: 8, needsCheck: false, product: P[2], nextProduct: P[6] },
      { slotNumber: 4, capacity: 10, qty: 0, needsCheck: false, product: P[3], nextProduct: null },
      { slotNumber: 5, capacity: 10, qty: 9, needsCheck: false, product: P[4], nextProduct: null },
      { slotNumber: 6, capacity: 10, qty: 4, needsCheck: false, product: P[5], nextProduct: null }
    ],
    transactions: {},
    restockSessions: {},
    operators: [{ username: 'operator', password: '123456', name: 'Operator Demo' }],
    failedLogins: {},
    lockedUntil: {},
    sessions: {}
  };

  function openCount(slotNumber) {
    return Object.keys(store.transactions).filter(function (id) {
      var t = store.transactions[id];
      return t.slotNumber === slotNumber && (t.status === 'PENDING' || t.status === 'PAID');
    }).length;
  }

  function expireOld() {
    var now = Date.now();
    Object.keys(store.transactions).forEach(function (id) {
      var t = store.transactions[id];
      if (t.status === 'PENDING' && now > t.expiresAt) t.status = 'EXPIRED';
    });
  }

  function isAvailable(s) {
    return !!s.product && s.product.isActive && !s.needsCheck && s.qty - openCount(s.slotNumber) > 0;
  }

  function catalog() {
    expireOld();
    return {
      catalogVersion: store.catalogVersion,
      machine: store.machine,
      slots: store.slots.map(function (s) {
        return { slotNumber: s.slotNumber, available: isAvailable(s), product: s.product };
      })
    };
  }

  function slotOf(n) {
    return store.slots.find(function (s) { return s.slotNumber === n; });
  }

  async function mockCall(fn, delay) {
    await sleep(delay === undefined ? 250 : delay);
    if (sim === 'offline') throw ApiError('OFFLINE', 'Server tidak terjangkau');
    return fn();
  }

  function requireSession(token) {
    if (!token || !store.sessions[token]) throw ApiError('UNAUTHORIZED', 'Sesi operator tidak valid');
    return store.sessions[token];
  }

  var mockApi = {
    health: function () { return mockCall(function () { return { ok: true }; }, 100); },

    getCatalog: function () { return mockCall(catalog); },

    createTransaction: function (p) {
      return mockCall(function () {
        expireOld();
        var existing = store.transactions[p.id];
        if (existing) return existing.response; // idempotent retry
        if (p.catalogVersion !== store.catalogVersion) {
          throw ApiError('CATALOG_STALE', 'Info produk diperbarui', { catalog: catalog() });
        }
        var s = slotOf(p.slotNumber);
        if (!s || !isAvailable(s)) throw ApiError('SOLD_OUT', 'Produk habis');
        var tx = {
          id: p.id, slotNumber: s.slotNumber, productId: s.product.id, price: s.product.price,
          status: 'PENDING', createdAt: Date.now(), expiresAt: Date.now() + 60000
        };
        tx.response = { id: tx.id, slotNumber: tx.slotNumber, product: s.product, amount: tx.price, expiresInSeconds: 60 };
        store.transactions[tx.id] = tx;
        return tx.response;
      });
    },

    pay: function (id) {
      return mockCall(async function () {
        var t = store.transactions[id];
        if (!t) throw ApiError('NOT_FOUND', 'Transaksi tidak ditemukan');
        if (t.status === 'DISPENSED' || t.status === 'FAILED') return { id: id, status: t.status, failureReason: t.failureReason || null };
        expireOld();
        if (t.status !== 'PENDING') throw ApiError('EXPIRED', 'Waktu pembayaran habis');
        t.status = 'PAID';
        await sleep(1800); // the coil turning
        var s = slotOf(t.slotNumber);
        if (sim === 'error') {
          t.status = 'FAILED';
          t.failureReason = 'MOTOR_TIMEOUT';
          s.needsCheck = true;
          store.catalogVersion += 1;
        } else {
          t.status = 'DISPENSED';
          s.qty -= 1;
        }
        return { id: id, status: t.status, failureReason: t.failureReason || null };
      });
    },

    cancel: function (id, reason) {
      return mockCall(function () {
        var t = store.transactions[id];
        if (t && t.status === 'PENDING') t.status = reason === 'TIMEOUT' ? 'EXPIRED' : 'CANCELLED';
        return { id: id, status: t ? t.status : null };
      }, 100);
    },

    report: function (id) {
      return mockCall(function () {
        var t = store.transactions[id];
        if (t && !t.customerReportedAt) t.customerReportedAt = Date.now();
        return { id: id, reported: true };
      });
    },

    login: function (username, password) {
      return mockCall(function () {
        var key = (username || '').toLowerCase();
        var lock = store.lockedUntil[key] || 0;
        if (Date.now() < lock) {
          throw ApiError('LOCKED', 'Akun terkunci', { retryInSeconds: Math.ceil((lock - Date.now()) / 1000) });
        }
        var op = store.operators.find(function (o) { return o.username === key && o.password === password; });
        if (!op) {
          store.failedLogins[key] = (store.failedLogins[key] || 0) + 1;
          if (store.failedLogins[key] >= 5) {
            store.failedLogins[key] = 0;
            store.lockedUntil[key] = Date.now() + 5 * 60000;
            throw ApiError('LOCKED', 'Akun terkunci', { retryInSeconds: 300 });
          }
          throw ApiError('INVALID_LOGIN', 'Username atau password salah', { attemptsLeft: 5 - store.failedLogins[key] });
        }
        store.failedLogins[key] = 0;
        var token = uuid();
        store.sessions[token] = { username: op.username, name: op.name, startedAt: new Date().toISOString() };
        return { token: token, name: op.name, username: op.username };
      }, 400);
    },

    logout: function (token) {
      return mockCall(function () { delete store.sessions[token]; return { ok: true }; }, 50);
    },

    getRestockSlots: function (token) {
      return mockCall(function () {
        requireSession(token);
        return {
          machine: store.machine,
          slots: store.slots.map(function (s) {
            return {
              slotNumber: s.slotNumber, capacity: s.capacity, qty: s.qty, needsCheck: s.needsCheck,
              product: s.product, nextProduct: s.nextProduct
            };
          })
        };
      });
    },

    saveRestock: function (token, session) {
      return mockCall(function () {
        var op = requireSession(token);
        if (store.restockSessions[session.id]) return store.restockSessions[session.id]; // idempotent retry
        session.items.forEach(function (it) {
          var s = slotOf(it.slotNumber);
          var ok = s && Number.isInteger(it.qtyAfter) && it.qtyAfter >= 0 && it.qtyAfter <= s.capacity;
          if (!ok) throw ApiError('INVALID_INPUT', 'Jumlah slot ' + it.slotNumber + ' tidak valid');
        });
        session.items.forEach(function (it) {
          var s = slotOf(it.slotNumber);
          if (s.nextProduct) { s.product = s.nextProduct; s.nextProduct = null; }
          s.qty = it.qtyAfter;
          s.needsCheck = false;
        });
        store.catalogVersion += 1;
        var result = { id: session.id, operator: op.username, savedAt: new Date().toISOString(), catalogVersion: store.catalogVersion };
        store.restockSessions[session.id] = result;
        return result;
      }, 600);
    }
  };

  // Helpers for demos and tests in mock mode, e.g. from the browser console:
  //   KioskApi.mock.changePrice(2, 40000)  -> next purchase of slot 2 hits CATALOG_STALE
  //   KioskApi.mock.setSim('error')        -> next dispense fails
  var mockTools = {
    store: store,
    setSim: function (v) { sim = v; },
    changePrice: function (slotNumber, price) {
      var s = slotOf(slotNumber);
      s.product = Object.assign({}, s.product, { price: price });
      store.catalogVersion += 1;
    }
  };

  global.KioskApi = Object.assign({}, cfg.mock ? mockApi : realApi, {
    isMock: cfg.mock,
    uuid: uuid,
    ApiError: ApiError,
    mock: cfg.mock ? mockTools : null
  });
})(window);
