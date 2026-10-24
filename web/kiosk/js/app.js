/*
 * Kiosk UI: six screens in one page.
 *   home -> detail -> payment            (customer)
 *   home -(hold secret 3 s)-> login -> restock -> restock-confirm   (operator)
 */
(function () {
  'use strict';

  var api = window.KioskApi;

  var CONFIG = {
    secretHoldMs: 3000,        // hold the camouflaged square this long to open the login
    detailIdleMs: 30000,       // detail screen returns home after 30 s untouched
    operatorIdleMs: 120000,    // operator screens log out after 2 min untouched
    reportWindowS: 60,         // "Produk tidak keluar?" stays available for 60 s
    resultScreenMs: 15000,     // failure / report screens return home after 15 s
    offlineRetryMs: 5000,      // ping interval while offline
    homeRefreshMs: 30000,      // on the home screen, re-read the catalog the agent keeps (a local call)
    dailyReloadHour: 3         // reload the catalog every day at 03:00
  };

  /* ------------------------------------------------------------ helpers */

  var $ = function (sel, root) { return (root || document).querySelector(sel); };
  var rupiahFmt = new Intl.NumberFormat('id-ID', { style: 'currency', currency: 'IDR', maximumFractionDigits: 0 });
  var rupiah = function (n) { return rupiahFmt.format(n); };
  var shortId = function (id) { return String(id).replace(/-/g, '').slice(0, 8).toUpperCase(); };

  function el(tag, attrs, children) {
    var node = document.createElement(tag);
    Object.keys(attrs || {}).forEach(function (k) {
      var v = attrs[k];
      if (k === 'class') node.className = v;
      else if (k === 'text') node.textContent = v;
      else if (k.slice(0, 2) === 'on') node.addEventListener(k.slice(2), v);
      else if (v === true) node.setAttribute(k, '');
      else if (v !== false && v != null) node.setAttribute(k, v);
    });
    (children || []).forEach(function (c) { if (c) node.appendChild(typeof c === 'string' ? document.createTextNode(c) : c); });
    return node;
  }

  function productImage(product, cls) {
    var img = el('img', { class: cls || '', alt: product ? product.name : '', draggable: 'false' });
    img.src = (product && product.imageUrl) || '';
    img.onerror = function () { img.onerror = null; img.removeAttribute('src'); img.classList.add('img-missing'); };
    return img;
  }

  var toastTimer = null;
  function toast(msg, ms) {
    var t = $('#toast');
    t.textContent = msg;
    t.classList.add('show');
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { t.classList.remove('show'); }, ms || 3500);
  }

  function busy(on, text) {
    $('#busy-text').textContent = text || 'Memproses…';
    $('#busy').hidden = !on;
  }

  /* ------------------------------------------------------------ state */

  var state = {
    screen: 'home',
    catalog: null,
    selected: null,      // slot shown on the detail screen
    tx: null,            // current transaction
    payTimer: null,
    resultTimer: null,
    operator: null,      // { token, name, startedAt }
    restock: null,       // { machine, items: [...] , sessionId }
    reloadPending: false,
    offline: false
  };

  /* ------------------------------------------------------------ navigation & idle */

  var idleTimer = null;

  function show(name) {
    document.querySelectorAll('.screen').forEach(function (s) {
      s.classList.toggle('active', s.dataset.screen === name);
    });
    state.screen = name;
    resetIdle();
  }

  function resetIdle() {
    clearTimeout(idleTimer);
    var ms = 0;
    if (state.screen === 'detail') ms = CONFIG.detailIdleMs;
    if (state.screen === 'login' || state.screen === 'restock' || state.screen === 'restock-confirm') ms = CONFIG.operatorIdleMs;
    if (ms) idleTimer = setTimeout(onIdle, ms);
  }

  function onIdle() {
    if (state.screen === 'detail') goHome();
    else logout('Sesi operator berakhir karena tidak ada aktivitas');
  }

  function goHome() {
    clearInterval(state.payTimer);
    clearTimeout(state.resultTimer);
    $('#pay-overlay').hidden = true;
    state.tx = null;
    state.selected = null;
    show('home');
    if (state.reloadPending) loadCatalog();
  }

  /* ------------------------------------------------------------ errors & offline */

  var offlineTimer = null;

  // detail: why the server refuses this machine (a wrong key, or deactivated in
  // the admin web), shown under "Mesin offline"; empty for a plain network problem.
  function setOffline(on, detail) {
    var d = $('#offline-detail');
    d.textContent = (on && detail) || '';
    d.hidden = !(on && detail);
    if (state.offline === on) return;
    state.offline = on;
    $('#offline').hidden = !on;
    clearInterval(offlineTimer);
    if (on) {
      offlineTimer = setInterval(function () {
        api.health().then(function () { setOffline(false); loadCatalog(); }).catch(function (err) {
          if (err && err.code === 'OFFLINE') setOffline(true, offlineDetail(err));
        });
      }, CONFIG.offlineRetryMs);
    }
  }

  function offlineDetail(err) {
    return err && err.data && err.data.reason ? err.data.message : '';
  }

  function handleError(err) {
    if (err && err.code === 'OFFLINE') { setOffline(true, offlineDetail(err)); return; }
    console.error(err);
    toast((err && err.message) || 'Terjadi kesalahan, coba lagi');
  }

  /* ------------------------------------------------------------ 1. home */

  async function loadCatalog() {
    try {
      state.catalog = await api.getCatalog();
      state.reloadPending = false;
      setOffline(false);
      renderHome();
    } catch (err) {
      handleError(err);
    }
  }

  function renderHome() {
    var grid = $('#product-grid');
    grid.textContent = '';
    (state.catalog ? state.catalog.slots : []).forEach(function (slot) {
      var p = slot.product;
      var card = el('button', {
        class: 'product-card' + (slot.available ? '' : ' is-soldout'),
        type: 'button',
        disabled: !slot.available,
        'aria-label': p ? p.name + ', ' + rupiah(p.price) : 'Slot ' + slot.slotNumber + ' kosong',
        onclick: function () { openDetail(slot); }
      }, [
        el('span', { class: 'slot-badge', text: String(slot.slotNumber) }),
        el('span', { class: 'card-media' }, [p ? productImage(p) : null, slot.available ? null : el('span', { class: 'soldout', text: 'Habis' })]),
        el('span', { class: 'card-name', text: p ? p.name : 'Kosong' }),
        el('span', { class: 'card-price', text: p ? rupiah(p.price) : '' })
      ]);
      grid.appendChild(card);
    });
  }

  // Camouflaged operator entry: hold for CONFIG.secretHoldMs.
  (function bindSecret() {
    var btn = $('#secret');
    var timer = null;
    var start = function (e) {
      e.preventDefault();
      clearTimeout(timer);
      timer = setTimeout(openLogin, CONFIG.secretHoldMs);
    };
    var stop = function () { clearTimeout(timer); };
    btn.addEventListener('pointerdown', start);
    ['pointerup', 'pointerleave', 'pointercancel'].forEach(function (ev) { btn.addEventListener(ev, stop); });
  })();

  /* ------------------------------------------------------------ 2. detail */

  function openDetail(slot) {
    if (!slot.available || !slot.product) return;
    state.selected = slot;
    var p = slot.product;
    var img = $('#detail-img');
    img.classList.remove('img-missing');
    img.src = p.imageUrl || '';
    img.alt = p.name;
    $('#detail-name').textContent = p.name;
    $('#detail-desc').textContent = p.description || '';
    $('#detail-price').textContent = rupiah(p.price);
    $('#btn-buy').disabled = false;
    show('detail');
    // FR-10: a tap is when a customer is about to buy, so check the server now;
    // if it is gone, "Mesin offline" covers the screen before "Bayar".
    api.health().catch(function (err) { if (err && err.code === 'OFFLINE') handleError(err); });
  }

  $('#detail-img').onerror = function () { this.removeAttribute('src'); this.classList.add('img-missing'); };

  $('#btn-buy').addEventListener('click', async function () {
    var slot = state.selected;
    if (!slot) return;
    this.disabled = true;
    busy(true, 'Menyiapkan pembayaran…');
    try {
      var tx = await api.createTransaction({
        id: api.uuid(),
        slotNumber: slot.slotNumber,
        catalogVersion: state.catalog.catalogVersion
      });
      busy(false);
      openPayment(tx);
    } catch (err) {
      busy(false);
      if (err.code === 'CATALOG_STALE') {
        state.catalog = err.data.catalog || state.catalog;
        renderHome();
        goHome();
        toast('Info produk diperbarui. Silakan pilih lagi.');
        if (!err.data.catalog) loadCatalog();
      } else if (err.code === 'SOLD_OUT') {
        goHome();
        toast('Maaf, produk ini sudah habis.');
        loadCatalog();
      } else {
        goHome();
        handleError(err);
      }
    }
  });

  /* ------------------------------------------------------------ 3. payment */

  function openPayment(tx) {
    state.tx = tx;
    $('#qr').innerHTML = fakeQr(tx.id);
    $('#pay-product').textContent = tx.product.name;
    $('#pay-amount').textContent = rupiah(tx.amount);
    $('#btn-pay').disabled = false;
    $('#pay-overlay').hidden = true;
    var left = tx.expiresInSeconds || 60;
    $('#pay-timer').textContent = left;
    clearInterval(state.payTimer);
    state.payTimer = setInterval(function () {
      left -= 1;
      $('#pay-timer').textContent = Math.max(left, 0);
      if (left <= 0) {
        clearInterval(state.payTimer);
        api.cancel(tx.id, 'TIMEOUT').catch(function () {});
        goHome();
        toast('Waktu pembayaran habis.');
      }
    }, 1000);
    show('payment');
  }

  $('#pay-back').addEventListener('click', function () {
    if (!state.tx) return goHome();
    var id = state.tx.id;
    clearInterval(state.payTimer);
    api.cancel(id, 'CANCEL').catch(function () {});
    goHome();
  });

  $('#btn-pay').addEventListener('click', async function () {
    var tx = state.tx;
    if (!tx) return;
    this.disabled = true;
    clearInterval(state.payTimer);
    payOverlay([
      el('div', { class: 'spinner' }),
      el('p', { class: 'overlay-title', text: 'Produk sedang dikeluarkan…' }),
      el('p', { class: 'overlay-text', text: 'Mohon tunggu sebentar.' })
    ]);
    try {
      var res = await api.pay(tx.id);
      if (res.status === 'DISPENSED') showDispensed(tx);
      else showFailed(tx, res.failureReason);
    } catch (err) {
      if (err.code === 'EXPIRED') {
        goHome();
        toast('Waktu pembayaran habis.');
      } else if (err.code === 'OFFLINE') {
        showFailed(tx, 'OFFLINE');
      } else {
        showFailed(tx, err.code);
      }
    }
  });

  function payOverlay(children) {
    var body = $('#pay-overlay-body');
    body.textContent = '';
    children.forEach(function (c) { if (c) body.appendChild(c); });
    $('#pay-overlay').hidden = false;
  }

  function showDispensed(tx) {
    state.reloadPending = true; // this sale may have emptied the slot; refresh before the next customer
    var left = CONFIG.reportWindowS;
    var counter = el('span', { text: String(left) });
    payOverlay([
      el('div', { class: 'icon-circle icon-ok', text: '✓' }),
      el('p', { class: 'overlay-title', text: 'Silakan ambil produk' }),
      el('p', { class: 'overlay-text', text: 'Produk ada di kotak pengambilan di bawah.' }),
      el('button', { class: 'btn btn-primary btn-wide', type: 'button', text: 'Selesai', onclick: goHome }),
      el('button', { class: 'btn-link', type: 'button', onclick: function () { reportProblem(tx); } },
        ['Produk tidak keluar? (', counter, ' dtk)'])
    ]);
    clearInterval(state.payTimer);
    state.payTimer = setInterval(function () {
      left -= 1;
      counter.textContent = String(Math.max(left, 0));
      if (left <= 0) goHome();
    }, 1000);
  }

  async function reportProblem(tx) {
    clearInterval(state.payTimer);
    busy(true, 'Mengirim laporan…');
    try {
      await api.report(tx.id);
      busy(false);
      showResult('icon-warn', '!', 'Laporan diterima',
        'Maaf atas ketidaknyamanannya. Simpan ID transaksi ini untuk petugas:', shortId(tx.id));
    } catch (err) {
      busy(false);
      showResult('icon-warn', '!', 'Laporan belum terkirim',
        'Silakan hubungi petugas dan sebutkan ID transaksi ini:', shortId(tx.id));
    }
  }

  function showFailed(tx, reason) {
    var text = reason === 'OFFLINE'
      ? 'Koneksi terputus. Jika produk tidak keluar, hubungi petugas dengan ID ini:'
      : 'Produk tidak dapat dikeluarkan. Hubungi petugas dengan ID transaksi ini:';
    showResult('icon-err', '×', 'Maaf, terjadi gangguan', text, shortId(tx.id));
    state.reloadPending = true; // the slot is now blocked; refresh before the next customer
  }

  function showResult(iconClass, icon, title, text, code) {
    payOverlay([
      el('div', { class: 'icon-circle ' + iconClass, text: icon }),
      el('p', { class: 'overlay-title', text: title }),
      el('p', { class: 'overlay-text', text: text }),
      el('p', { class: 'tx-code', text: code }),
      el('button', { class: 'btn btn-primary btn-wide', type: 'button', text: 'Kembali ke awal', onclick: goHome })
    ]);
    clearTimeout(state.resultTimer);
    state.resultTimer = setTimeout(goHome, CONFIG.resultScreenMs);
  }

  // A QR-looking pattern for the simulation. It encodes nothing and cannot be paid.
  function fakeQr(seedText) {
    var n = 25;
    var seed = 0;
    for (var i = 0; i < seedText.length; i++) seed = (seed * 31 + seedText.charCodeAt(i)) >>> 0;
    var rand = function () {
      seed = (seed + 0x6D2B79F5) >>> 0;
      var t = seed;
      t = Math.imul(t ^ (t >>> 15), t | 1);
      t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
      return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
    };
    var finder = function (x, y) {
      var inBox = function (fx, fy) { return x >= fx && x < fx + 7 && y >= fy && y < fy + 7; };
      var corners = [[0, 0], [n - 7, 0], [0, n - 7]];
      for (var c = 0; c < corners.length; c++) {
        var fx = corners[c][0], fy = corners[c][1];
        if (inBox(fx, fy)) {
          var dx = x - fx, dy = y - fy;
          var ring = Math.min(dx, dy, 6 - dx, 6 - dy);
          return ring === 0 || ring >= 2 ? 1 : 0;
        }
        if (x >= fx - 1 && x <= fx + 7 && y >= fy - 1 && y <= fy + 7) return 0; // separator
      }
      return -1;
    };
    var rects = '';
    for (var y = 0; y < n; y++) {
      for (var x = 0; x < n; x++) {
        var f = finder(x, y);
        var dark = f === -1 ? rand() < 0.5 : f === 1;
        if (dark) rects += '<rect x="' + (x + 2) + '" y="' + (y + 2) + '" width="1.02" height="1.02"/>';
      }
    }
    return '<svg viewBox="0 0 29 29" role="img" aria-label="Kode QR simulasi" shape-rendering="crispEdges">' +
      '<rect width="29" height="29" fill="#fff"/><g fill="#111">' + rects + '</g></svg>';
  }

  /* ------------------------------------------------------------ 4. login */

  var activeField = null;

  function openLogin() {
    $('#login-user').value = '';
    $('#login-pass').value = '';
    $('#login-error').textContent = '';
    setActiveField($('#login-user'));
    renderKeyboard();
    show('login');
  }

  function setActiveField(input) {
    activeField = input;
    document.querySelectorAll('[data-field]').forEach(function (f) { f.classList.toggle('is-active', f === input); });
  }

  document.querySelectorAll('[data-field]').forEach(function (f) {
    f.addEventListener('pointerdown', function (e) { e.preventDefault(); setActiveField(f); });
  });

  var shift = false;
  var KEY_ROWS = ['1234567890', 'qwertyuiop', 'asdfghjkl', 'zxcvbnm'];

  function renderKeyboard() {
    var kb = $('#keyboard');
    kb.textContent = '';
    KEY_ROWS.forEach(function (row, r) {
      var rowEl = el('div', { class: 'kb-row' });
      if (r === 3) rowEl.appendChild(el('button', { class: 'key key-wide' + (shift ? ' is-on' : ''), type: 'button', text: '⇧', 'data-key': 'shift' }));
      row.split('').forEach(function (ch) {
        var c = shift ? ch.toUpperCase() : ch;
        rowEl.appendChild(el('button', { class: 'key', type: 'button', text: c, 'data-key': c }));
      });
      if (r === 3) rowEl.appendChild(el('button', { class: 'key key-wide', type: 'button', text: '⌫', 'data-key': 'back', 'aria-label': 'Hapus' }));
      kb.appendChild(rowEl);
    });
    kb.appendChild(el('div', { class: 'kb-row' }, [
      el('button', { class: 'key key-action', type: 'button', text: 'Hapus semua', 'data-key': 'clear' }),
      el('button', { class: 'key key-action', type: 'button', text: 'Berikutnya', 'data-key': 'next' })
    ]));
  }

  $('#keyboard').addEventListener('pointerdown', function (e) {
    var key = e.target.closest('[data-key]');
    if (!key) return;
    e.preventDefault();
    var k = key.dataset.key;
    if (!activeField) setActiveField($('#login-user'));
    var f = activeField;
    if (k === 'shift') { shift = !shift; renderKeyboard(); return; }
    if (k === 'back') f.value = f.value.slice(0, -1);
    else if (k === 'clear') f.value = '';
    else if (k === 'next') setActiveField(f.id === 'login-user' ? $('#login-pass') : $('#login-user'));
    else if (f.value.length < Number(f.maxLength || 32)) f.value += k;
    $('#login-error').textContent = '';
  });

  $('#login-form').addEventListener('submit', async function (e) {
    e.preventDefault();
    var user = $('#login-user').value.trim();
    var pass = $('#login-pass').value;
    if (!user || !pass) {
      $('#login-error').textContent = 'Isi username dan password.';
      return;
    }
    $('#btn-login').disabled = true;
    busy(true, 'Memeriksa akun…');
    try {
      var res = await api.login(user, pass);
      state.operator = { token: res.token, name: res.name, startedAt: new Date().toISOString() };
      await openRestock();
    } catch (err) {
      busy(false);
      if (err.code === 'INVALID_LOGIN') {
        var left = err.data.attemptsLeft;
        $('#login-error').textContent = 'Username atau password salah.' + (left != null ? ' Sisa percobaan: ' + left + '.' : '');
        $('#login-pass').value = '';
        setActiveField($('#login-pass'));
      } else if (err.code === 'LOCKED') {
        var min = Math.ceil((err.data.retryInSeconds || 300) / 60);
        $('#login-error').textContent = 'Akun terkunci. Coba lagi dalam ' + min + ' menit.';
      } else if (err.code === 'OFFLINE') {
        $('#login-error').textContent = 'Server tidak terjangkau. Login membutuhkan koneksi.';
      } else {
        $('#login-error').textContent = err.message || 'Login gagal.';
      }
    } finally {
      $('#btn-login').disabled = false;
    }
  });

  /* ------------------------------------------------------------ 5. restock */

  async function openRestock() {
    busy(true, 'Memuat data slot…');
    try {
      var data = await api.getRestockSlots(state.operator.token);
      state.restock = {
        sessionId: api.uuid(),
        machine: data.machine,
        items: data.slots.map(function (s) {
          return {
            slotNumber: s.slotNumber,
            capacity: s.capacity,
            before: s.qty,
            after: s.qty,               // starts at the current count; the operator sets the real one
            product: s.nextProduct || s.product,
            previousProduct: s.nextProduct ? s.product : null,
            needsCheck: s.needsCheck
          };
        })
      };
      busy(false);
      $('#restock-machine').textContent = data.machine ? '· ' + data.machine.code : '';
      $('#restock-operator').textContent = 'Operator: ' + state.operator.name;
      renderRestock();
      show('restock');
    } catch (err) {
      busy(false);
      if (err.code === 'UNAUTHORIZED') logout('Sesi operator tidak valid');
      else handleError(err);
    }
  }

  function renderRestock() {
    var grid = $('#slot-grid');
    grid.textContent = '';
    state.restock.items.forEach(function (it) {
      var hasProduct = !!it.product;
      var value = el('span', { class: 'stepper-value', 'aria-live': 'polite' });
      var minus = el('button', { class: 'step step-minus', type: 'button', text: '−', 'aria-label': 'Kurangi slot ' + it.slotNumber });
      var plus = el('button', { class: 'step step-plus', type: 'button', text: '+', 'aria-label': 'Tambah slot ' + it.slotNumber });
      var card = el('div', { class: 'slot-card' + (hasProduct ? '' : ' is-empty') }, [
        el('div', { class: 'slot-top' }, [
          el('span', { class: 'slot-badge', text: String(it.slotNumber) }),
          it.previousProduct ? el('span', { class: 'tag tag-change', text: 'Ganti produk' }) : null,
          it.needsCheck ? el('span', { class: 'tag tag-check', text: 'Perlu dicek' }) : null
        ]),
        el('div', { class: 'slot-media' }, [hasProduct ? productImage(it.product) : null]),
        el('p', { class: 'slot-name', text: hasProduct ? it.product.name : 'Belum ada produk' }),
        el('p', { class: 'slot-meta', text: hasProduct ? 'Sebelumnya ' + it.before + ' · Maks ' + it.capacity : '' }),
        el('div', { class: 'stepper' }, [minus, value, plus])
      ]);
      var sync = function () {
        value.textContent = String(it.after);
        minus.disabled = !hasProduct || it.after <= 0;
        plus.disabled = !hasProduct || it.after >= it.capacity;
        card.classList.toggle('is-changed', it.after !== it.before || !!it.previousProduct);
      };
      bindRepeat(minus, function () { if (it.after > 0) { it.after -= 1; sync(); } });
      bindRepeat(plus, function () { if (it.after < it.capacity) { it.after += 1; sync(); } });
      sync();
      grid.appendChild(card);
    });
  }

  // Tap = one step; hold = repeat.
  function bindRepeat(btn, fn) {
    var delay = null, repeat = null;
    var stop = function () { clearTimeout(delay); clearInterval(repeat); };
    btn.addEventListener('pointerdown', function (e) {
      if (btn.disabled) return;
      e.preventDefault();
      fn();
      delay = setTimeout(function () { repeat = setInterval(function () { if (btn.disabled) stop(); else fn(); }, 120); }, 450);
    });
    ['pointerup', 'pointerleave', 'pointercancel'].forEach(function (ev) { btn.addEventListener(ev, stop); });
  }

  $('#btn-restock-save').addEventListener('click', function () {
    renderConfirm();
    show('restock-confirm');
  });

  $('#btn-logout').addEventListener('click', function () { logout(); });

  function logout(message) {
    var op = state.operator;
    state.operator = null;
    state.restock = null;
    if (op) api.logout(op.token).catch(function () {});
    goHome();
    loadCatalog();
    if (message) toast(message);
  }

  /* ------------------------------------------------------------ 6. restock confirmation */

  function renderConfirm() {
    var list = $('#confirm-list');
    list.textContent = '';
    $('#confirm-error').textContent = '';
    var added = 0, changed = 0;
    state.restock.items.forEach(function (it) {
      var diff = it.after - it.before;
      if (diff > 0) added += diff;
      if (diff !== 0 || it.previousProduct) changed += 1;
      var diffText = diff > 0 ? '+' + diff : diff < 0 ? '−' + Math.abs(diff) : 'tetap';
      list.appendChild(el('div', { class: 'confirm-row' + (diff !== 0 || it.previousProduct ? ' is-changed' : '') }, [
        el('span', { class: 'slot-badge', text: String(it.slotNumber) }),
        el('div', { class: 'confirm-name' }, [
          el('span', { text: it.product ? it.product.name : 'Belum ada produk' }),
          it.previousProduct ? el('small', { text: 'Ganti dari ' + it.previousProduct.name }) : null
        ]),
        el('span', { class: 'confirm-qty', text: it.before + ' → ' + it.after }),
        el('span', { class: 'confirm-diff ' + (diff > 0 ? 'up' : diff < 0 ? 'down' : 'same'), text: diffText })
      ]));
    });
    $('#confirm-summary').textContent = changed
      ? changed + ' slot berubah · ' + added + ' item masuk'
      : 'Tidak ada perubahan jumlah';
  }

  $('#btn-confirm-back').addEventListener('click', function () { show('restock'); });

  $('#btn-confirm-commit').addEventListener('click', async function () {
    var r = state.restock;
    if (!r || !state.operator) return;
    var btn = this;
    btn.disabled = true;
    $('#confirm-error').textContent = '';
    busy(true, 'Menyimpan restock…');
    try {
      // Same session id on every retry, so a repeated save is recorded only once.
      await api.saveRestock(state.operator.token, {
        id: r.sessionId,
        startedAt: state.operator.startedAt,
        finishedAt: new Date().toISOString(),
        items: r.items.map(function (it) {
          return { slotNumber: it.slotNumber, qtyBefore: it.before, qtyAfter: it.after };
        })
      });
      busy(false);
      logout('Restock tersimpan. Terima kasih!');
    } catch (err) {
      busy(false);
      if (err.code === 'OFFLINE') {
        $('#confirm-error').textContent = 'Gagal menyimpan ke server. Data belum hilang: periksa koneksi lalu tekan Konfirmasi lagi.';
      } else if (err.code === 'UNAUTHORIZED') {
        logout('Sesi operator berakhir. Silakan login lagi.');
      } else {
        $('#confirm-error').textContent = err.message || 'Gagal menyimpan.';
      }
    } finally {
      btn.disabled = false;
    }
  });

  /* ------------------------------------------------------------ wiring */

  document.addEventListener('click', function (e) {
    var a = e.target.closest('[data-action="home"]');
    if (a) goHome();
  });

  document.addEventListener('pointerdown', resetIdle, true);
  document.addEventListener('contextmenu', function (e) { e.preventDefault(); }); // long-press menus off

  function scheduleDailyReload() {
    var now = new Date();
    var next = new Date(now);
    next.setHours(CONFIG.dailyReloadHour, 0, 0, 0);
    if (next <= now) next.setDate(next.getDate() + 1);
    setTimeout(function () {
      if (state.screen === 'home') loadCatalog();
      else state.reloadPending = true;
      scheduleDailyReload();
    }, next - now);
  }

  // While the home screen is idle, re-read the catalog from the agent (a local
  // call: the agent answers from its cache and talks to the server only when
  // the catalog changed, FR-02). A version the heartbeat found, or an
  // availability change, shows up without a customer touching anything.
  function scheduleHomeRefresh() {
    setInterval(function () {
      if (state.screen !== 'home' || state.offline || !$('#busy').hidden || !state.catalog) return;
      api.getCatalog().then(function (c) {
        if (state.screen !== 'home') return;
        if (JSON.stringify(c) !== JSON.stringify(state.catalog)) {
          state.catalog = c;
          renderHome();
        }
      }).catch(function (err) { if (err && err.code === 'OFFLINE') handleError(err); });
    }, CONFIG.homeRefreshMs);
  }

  if (new URLSearchParams(location.search).has('debug') || (window.KIOSK_CONFIG || {}).debug) document.body.classList.add('debug');

  // Small hook for automated tests and the thesis demo.
  window.Kiosk = { state: state, show: show, loadCatalog: loadCatalog, openLogin: openLogin, config: CONFIG };

  show('home');
  loadCatalog();
  scheduleDailyReload();
  if (!api.isMock) scheduleHomeRefresh();
})();
