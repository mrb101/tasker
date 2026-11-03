// tasker web UI: forms → JSON API, drag & drop triage, live refresh.
(() => {
  const $ = (s, el = document) => el.querySelector(s);
  const $$ = (s, el = document) => [...el.querySelectorAll(s)];
  const project = () => document.body.dataset.project || '';

  function actor() {
    const m = document.cookie.match(/(?:^|; )tasker_actor=([^;]*)/);
    return m ? decodeURIComponent(m[1]) : 'human';
  }

  async function api(spec, body) {
    const i = spec.indexOf(' ');
    const method = spec.slice(0, i), path = spec.slice(i + 1);
    const res = await fetch(path, {
      method,
      headers: { 'Content-Type': 'application/json', 'X-Tasker-Actor': actor() },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    if (res.status === 204) return null;
    const data = await res.json().catch(() => null);
    if (!res.ok) throw new Error((data && data.error) || res.statusText);
    return data;
  }

  let toastTimer;
  function toast(msg, err) {
    const t = $('#toast');
    t.textContent = msg;
    t.className = err ? 'err' : '';
    t.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => (t.hidden = true), err ? 6000 : 1800);
  }

  function fieldValue(el) {
    if (el.type === 'checkbox') return el.checked;
    const v = el.value;
    switch (el.dataset.type) {
      case 'int': return v === '' ? 0 : parseInt(v, 10);
      case 'list': return v.split(',').map((s) => s.trim()).filter(Boolean);
    }
    return v;
  }

  function formData(form, only) {
    const out = {};
    for (const el of form.elements) {
      if (!el.name || el.disabled || (only && el !== only)) continue;
      const v = fieldValue(el);
      if (el.hasAttribute('data-omit-empty') && (v === '' || (Array.isArray(v) && !v.length))) continue;
      const parts = el.name.split('.');
      let o = out;
      while (parts.length > 1) { const k = parts.shift(); o = o[k] = o[k] || {}; }
      o[parts[0]] = v;
    }
    return out;
  }

  function after(spec, res, form) {
    spec = spec || 'reload';
    if (spec === 'none') return;
    if (spec === 'reset') {
      form.reset();
      refresh().then(() => { const f = form.id ? document.getElementById(form.id) : null; const i = f && f.querySelector('input,textarea'); i && i.focus(); });
      return;
    }
    if (spec.startsWith('goto:')) {
      location.href = spec.slice(5).replace(/\{(\w+)\}/g, (_, k) => (res && res[k] != null ? encodeURIComponent(res[k]) : ''));
      return;
    }
    refresh();
  }

  // ---- forms ----
  document.addEventListener('submit', async (e) => {
    const form = e.target.closest('form[data-api]');
    if (!form) return;
    e.preventDefault();
    const btns = $$('button', form);
    btns.forEach((b) => (b.disabled = true));
    try {
      const res = await api(form.dataset.api, formData(form));
      if (form.dataset.then === 'reset') {
        const keep = $$('select', form).map((s) => s.value);
        form.reset();
        $$('select', form).forEach((s, i) => (s.value = keep[i]));
        await refresh();
        const again = $('form.quickadd input[name=title]');
        again && again.focus();
      } else {
        after(form.dataset.then, res, form);
      }
    } catch (err) {
      toast(err.message, true);
    } finally {
      btns.forEach((b) => (b.disabled = false));
    }
  });

  document.addEventListener('change', async (e) => {
    const form = e.target.closest('form[data-autosubmit]');
    if (!form || !e.target.name) return;
    try {
      await api(form.dataset.api, formData(form, e.target));
      if (form.hasAttribute('data-peek-form')) {
        afterPeekChange(e.target);
      } else {
        toast('Saved');
      }
      refresh();
    } catch (err) {
      toast(err.message, true);
      refresh();
    }
  });

  // ---- buttons & toggles ----
  document.addEventListener('click', async (e) => {
    const tog = e.target.closest('[data-toggle]');
    if (tog) {
      e.preventDefault();
      const t = $(tog.dataset.toggle);
      if (t) {
        t.hidden = !t.hidden;
        if (!t.hidden) { const f = t.querySelector('input:not([type=hidden]),textarea,select'); f && f.focus(); }
      }
      return;
    }
    const btn = e.target.closest('button[data-api]');
    if (!btn) return;
    e.preventDefault();
    if (btn.dataset.confirm && !btn.classList.contains('arm')) {
      btn.classList.add('arm');
      btn.dataset.label = btn.textContent;
      btn.textContent = 'Click again to confirm';
      btn.title = btn.dataset.confirm;
      toast(btn.dataset.confirm);
      setTimeout(() => { btn.classList.remove('arm'); btn.textContent = btn.dataset.label; }, 4000);
      return;
    }
    btn.disabled = true;
    try {
      const body = btn.dataset.body ? JSON.parse(btn.dataset.body) : undefined;
      const res = await api(btn.dataset.api, body);
      after(btn.dataset.then, res);
    } catch (err) {
      toast(err.message, true);
    } finally {
      btn.disabled = false;
    }
  });

  document.addEventListener('dblclick', (e) => {
    const el = e.target.closest('[data-toggle-dbl]');
    if (el && !e.target.closest('a')) { const t = $(el.dataset.toggleDbl); if (t) { t.hidden = false; const f = t.querySelector('textarea'); f && f.focus(); } }
  });

  // ---- quick view: click a card to read it in a dialog, not a new page ----
  const peek = { list: [], idx: -1 };

  async function openPeek(ref, list) {
    if (list) peek.list = list;
    peek.idx = peek.list.indexOf(ref);
    const dlg = $('#peek');
    const res = await fetch('/i/' + encodeURIComponent(ref) + '/peek');
    if (!res.ok) { toast(await res.text(), true); return; }
    dlg.innerHTML = await res.text();
    const pos = $('.peek-pos', dlg);
    if (pos && peek.idx >= 0) pos.textContent = `${peek.idx + 1} of ${peek.list.length}`;
    $$('.peek-nav', dlg).forEach((b) => {
      b.disabled = peek.idx < 0 || (b.dataset.peek === 'prev' ? peek.idx === 0 : peek.idx === peek.list.length - 1);
    });
    if (!dlg.open) dlg.showModal();
    const sc = $('.peek-scroll', dlg);
    if (sc) sc.scrollTop = 0;
  }

  function stepPeek(d) {
    const i = peek.idx + d;
    if (peek.idx >= 0 && i >= 0 && i < peek.list.length) openPeek(peek.list[i]);
  }

  // After a change in the dialog: moving to another queue takes the item out of
  // the column being triaged, so go on to the next one; otherwise show it again.
  function afterPeekChange(el) {
    const ref = $('#peek .peek').dataset.ref;
    if (el.hasAttribute('data-advance')) {
      toast(`${ref} → ${el.value || 'triage'}`);
      const i = peek.list.indexOf(ref);
      if (i >= 0) {
        peek.list.splice(i, 1);
        if (peek.list.length) { openPeek(peek.list[Math.min(i, peek.list.length - 1)]); return; }
        $('#peek').close();
        return;
      }
    } else {
      toast('Saved');
    }
    openPeek(ref);
  }

  document.addEventListener('click', (e) => {
    const dlg = $('#peek');
    if (dlg.open && dlg.contains(e.target)) {
      if (e.target === dlg) { dlg.close(); return; } // backdrop
      const b = e.target.closest('[data-peek]');
      if (b) { e.preventDefault(); b.dataset.peek === 'close' ? dlg.close() : stepPeek(b.dataset.peek === 'next' ? 1 : -1); return; }
      const dep = e.target.closest('a[data-peek-ref]');
      if (dep && e.button === 0 && !(e.metaKey || e.ctrlKey || e.shiftKey)) { e.preventDefault(); openPeek(dep.dataset.peekRef); }
      return;
    }
    const card = e.target.closest('#main .card');
    if (!card || e.target.closest('.sel') || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    const a = e.target.closest('a');
    if (a && !a.classList.contains('card-title') && !a.closest('.card-top')) return; // epic chip etc. navigate
    e.preventDefault();
    const refs = [...card.parentElement.querySelectorAll(':scope > .card:not(.filtered)')].map((c) => c.dataset.ref);
    openPeek(card.dataset.ref, refs);
  });

  document.addEventListener('keydown', (e) => {
    const dlg = $('#peek');
    if (!dlg.open || /^(input|textarea|select)$/i.test(e.target.tagName)) return;
    if (e.key === 'ArrowRight' || e.key === 'j') { e.preventDefault(); stepPeek(1); }
    if (e.key === 'ArrowLeft' || e.key === 'k') { e.preventDefault(); stepPeek(-1); }
  });

  // ---- chart tooltips: any [data-tip] mark, on hover and on keyboard focus ----
  function showTip(el, x, y) {
    const tip = $('#tip');
    tip.textContent = el.dataset.tip; // labels are data: textContent, never innerHTML
    tip.hidden = false;
    const r = tip.getBoundingClientRect();
    let left = x + 14, top = y + 14;
    if (left + r.width > innerWidth - 8) left = x - r.width - 14;
    if (top + r.height > innerHeight - 8) top = y - r.height - 14;
    tip.style.left = Math.max(8, left) + 'px';
    tip.style.top = Math.max(8, top) + 'px';
  }
  function hideTip() { $('#tip').hidden = true; }
  document.addEventListener('pointermove', (e) => {
    const el = e.target.closest && e.target.closest('[data-tip]');
    if (el) showTip(el, e.clientX, e.clientY); else if (!$('#tip').hidden) hideTip();
  });
  document.addEventListener('focusin', (e) => {
    const el = e.target.closest && e.target.closest('[data-tip]');
    if (!el) return;
    const r = el.getBoundingClientRect();
    showTip(el, r.left + r.width / 2, r.top);
  });
  document.addEventListener('focusout', hideTip);
  document.addEventListener('scroll', hideTip, true);

  // ---- selection & bulk changes ----
  const sel = [];               // selected refs, in the order they were picked
  let lastPicked = null;        // for shift-click ranges

  function setSelected(ref, on) {
    const i = sel.indexOf(ref);
    if (on && i < 0) sel.push(ref);
    if (!on && i >= 0) sel.splice(i, 1);
  }

  function syncSelection() {
    const present = new Set($$('#main [data-sel]').map((c) => c.dataset.sel));
    for (let i = sel.length - 1; i >= 0; i--) if (!present.has(sel[i])) sel.splice(i, 1);
    $$('#main [data-sel]').forEach((c) => {
      c.checked = sel.includes(c.dataset.sel);
      const card = c.closest('.card, tr');
      if (card) card.classList.toggle('selected', c.checked);
    });
    const bar = $('#bulkbar');
    if (!bar) return;
    bar.hidden = sel.length === 0;
    $('.bulk-count', bar).textContent = sel.length === 1 ? '1 selected' : `${sel.length} selected`;
    if (!bar.hidden) loadBulkOptions();
  }

  // Queue and epic choices for the bar, fetched once per page.
  let bulkLoaded = false;
  async function loadBulkOptions() {
    if (bulkLoaded) return;
    bulkLoaded = true;
    try {
      const [queues, epics] = await Promise.all([
        api('GET /api/queues?project=' + project()),
        api('GET /api/projects/' + project() + '/epics'),
      ]);
      const qs = $('[data-bulk=queue]');
      queues.forEach((q) => { const o = document.createElement('option'); o.value = q.name; o.textContent = q.name; qs.appendChild(o); });
      const es = $('[data-bulk=epic]');
      epics.forEach((ep) => { const o = document.createElement('option'); o.value = ep.ref; o.textContent = ep.ref.split('-').pop() + ' · ' + ep.title; es.appendChild(o); });
    } catch (err) { bulkLoaded = false; }
  }

  // The checkboxes a "select all" control governs: its column's cards, or its table's rows.
  function scopeOf(el) {
    const table = el.closest('table');
    if (table) return $$('tbody [data-sel]', table);
    const col = el.closest('.col, .triage, .page');
    return col ? $$('.cards [data-sel]', col).filter((c) => !c.closest('.card.filtered')) : [];
  }

  document.addEventListener('click', (e) => {
    const box = e.target.closest('#main input[data-sel]');
    if (box) {
      e.stopPropagation();
      if (e.shiftKey && lastPicked) {
        const all = scopeOf(box);
        const a = all.findIndex((c) => c.dataset.sel === lastPicked), b = all.indexOf(box);
        if (a >= 0 && b >= 0) all.slice(Math.min(a, b), Math.max(a, b) + 1).forEach((c) => setSelected(c.dataset.sel, box.checked));
      }
      setSelected(box.dataset.sel, box.checked);
      lastPicked = box.dataset.sel;
      syncSelection();
      return;
    }
    const all = e.target.closest('#main [data-selall]');
    if (all) {
      const boxes = scopeOf(all);
      const on = all.type === 'checkbox' ? all.checked : !boxes.every((c) => sel.includes(c.dataset.sel));
      boxes.forEach((c) => setSelected(c.dataset.sel, on));
      syncSelection();
      return;
    }
    if (e.target.closest('[data-bulk-clear]')) { sel.length = 0; syncSelection(); return; }
    const lab = e.target.closest('[data-bulk-label-op]');
    if (lab) {
      const input = $('[data-bulk-label]');
      const v = input.value.trim();
      if (!v) { input.focus(); return; }
      bulkApply(lab.dataset.bulkLabelOp === 'add' ? { add_labels: [v] } : { remove_labels: [v] }, `label "${v}" ${lab.dataset.bulkLabelOp === 'add' ? 'added' : 'removed'}`)
        .then(() => (input.value = ''));
    }
  }, true);

  document.addEventListener('change', (e) => {
    const sel_ = e.target.closest('#bulkbar select[data-bulk]');
    if (sel_ && sel_.value) {
      const field = sel_.dataset.bulk;
      let value = sel_.value, what = `${field} → ${sel_.options[sel_.selectedIndex].textContent}`;
      if (value === '__triage' || value === '__none') value = '';
      sel_.value = '';
      bulkApply({ [field]: value }, what);
      return;
    }
    if (e.target.closest('form[data-autoget]')) e.target.closest('form').requestSubmit();
  });

  async function bulkApply(patch, what) {
    const refs = sel.slice();
    try {
      const res = await api(`POST /api/projects/${project()}/items/batch`, { refs, patch });
      toast(`${res.length} stor${res.length === 1 ? 'y' : 'ies'}: ${what}`);
      sel.length = 0;
      await refresh();
    } catch (err) {
      toast(err.message, true); // nothing changed: a bulk change is all or nothing
    }
  }

  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && sel.length && !$('#peek').open) { sel.length = 0; syncSelection(); }
  });

  // ---- sidebar: mobile toggle, close the project switcher on outside click ----
  document.addEventListener('click', (e) => {
    if (e.target.closest('#navtoggle')) { $('#sidenav').classList.toggle('open'); return; }
    const sw = $('.switcher[open]');
    if (sw && !e.target.closest('.switcher')) sw.open = false;
  });

  // ---- actor ----
  document.addEventListener('click', (e) => {
    const b = e.target.closest('#actor');
    if (!b) return;
    const input = document.createElement('input');
    input.value = actor();
    input.className = 'actor-input';
    input.size = 12;
    b.replaceWith(input);
    input.focus();
    input.select();
    const done = (save) => {
      if (save && input.value.trim()) {
        document.cookie = 'tasker_actor=' + encodeURIComponent(input.value.trim()) + '; path=/; max-age=31536000; samesite=lax';
      }
      location.reload();
    };
    input.addEventListener('keydown', (ev) => { if (ev.key === 'Enter') done(true); if (ev.key === 'Escape') done(false); });
    input.addEventListener('blur', () => done(true));
  });

  // ---- search: jump straight to references ----
  document.addEventListener('submit', (e) => {
    if (!e.target.classList.contains('search')) return;
    const q = e.target.q.value.trim();
    if (/^[A-Za-z][A-Za-z0-9]{0,9}-[EeDd]?\d+$/.test(q)) {
      e.preventDefault();
      location.href = '/r/' + q.toUpperCase();
    }
  }, true);

  // ---- hotkeys ----
  document.addEventListener('keydown', (e) => {
    if (e.metaKey || e.ctrlKey || e.altKey || $('#peek').open) return;
    const tag = (e.target.tagName || '').toLowerCase();
    if (tag === 'input' || tag === 'textarea' || tag === 'select') {
      if (e.key === 'Escape') e.target.blur();
      return;
    }
    const el = $(`[data-hotkey="${CSS.escape(e.key)}"]`);
    if (el && el.offsetParent !== null) { e.preventDefault(); el.focus(); }
  });

  // ---- board filter ----
  function applyFilter() {
    const f = $('#boardfilter');
    if (!f) return;
    const text = f.querySelector('[name=text]').value.toLowerCase();
    const type = f.querySelector('[name=type]').value;
    const epic = f.querySelector('[name=epic]').value;
    $$('#board .card').forEach((c) => {
      const hide = (text && !c.textContent.toLowerCase().includes(text)) || (type && c.dataset.type !== type) || (epic && c.dataset.epic !== epic);
      c.classList.toggle('filtered', !!hide);
    });
  }
  document.addEventListener('input', (e) => { if (e.target.closest('#boardfilter')) applyFilter(); });
  document.addEventListener('change', (e) => { if (e.target.closest('#boardfilter')) applyFilter(); });

  // ---- drag & drop triage ----
  let dragging = null, dragFrom = null, dropped = false;
  document.addEventListener('dragstart', (e) => {
    const card = e.target.closest && e.target.closest('.card[draggable]');
    if (!card) return;
    dragging = card;
    dragFrom = card.parentElement;
    dropped = false;
    e.dataTransfer.effectAllowed = 'move';
    e.dataTransfer.setData('text/plain', card.dataset.ref);
    requestAnimationFrame(() => card.classList.add('dragging'));
  });
  document.addEventListener('dragend', () => {
    if (!dragging) return;
    dragging.classList.remove('dragging');
    $$('.cards.over').forEach((c) => c.classList.remove('over'));
    if (!dropped) refresh();
    dragging = null;
  });
  document.addEventListener('dragover', (e) => {
    const zone = e.target.closest && e.target.closest('.cards.drop');
    if (!zone || !dragging) return;
    e.preventDefault();
    $$('.cards.over').forEach((c) => c !== zone && c.classList.remove('over'));
    zone.classList.add('over');
    if (zone.dataset.queue === '') { if (dragging.parentElement !== zone) zone.prepend(dragging); return; }
    const after = [...zone.querySelectorAll(':scope > .card:not(.dragging)')].find((c) => {
      const r = c.getBoundingClientRect();
      return e.clientY < r.top + r.height / 2;
    });
    if (after) zone.insertBefore(dragging, after); else zone.appendChild(dragging);
  });
  document.addEventListener('drop', async (e) => {
    const zone = e.target.closest && e.target.closest('.cards.drop');
    if (!zone || !dragging) return;
    e.preventDefault();
    dropped = true;
    const card = dragging;
    const ref = card.dataset.ref;
    const queue = zone.dataset.queue;
    try {
      if (queue === '') {
        if (dragFrom !== zone) await api('DELETE /api/queued/' + ref);
      } else {
        const next = card.nextElementSibling && card.nextElementSibling.classList.contains('card') ? card.nextElementSibling : null;
        const prev = card.previousElementSibling && card.previousElementSibling.classList.contains('card') ? card.previousElementSibling : null;
        let position = { where: 'bottom' };
        if (next) position = { where: 'before', anchor: next.dataset.ref };
        else if (prev) position = { where: 'after', anchor: prev.dataset.ref };
        await api(`POST /api/queues/${project()}/${queue}/items`, { refs: [ref], position });
      }
      toast(queue ? `${ref} → ${queue}` : `${ref} → triage`);
    } catch (err) {
      toast(err.message, true);
    }
    refresh();
  });

  // ---- refresh & live updates ----
  let refreshing = null;
  async function refresh() {
    if (refreshing) return refreshing;
    refreshing = (async () => {
      try {
        const res = await fetch(location.href, { headers: { 'X-Tasker-Actor': actor() } });
        if (!res.ok) return;
        const doc = new DOMParser().parseFromString(await res.text(), 'text/html');
        const board = $('#board');
        const sx = board ? board.scrollLeft : 0;
        const cols = $$('#board .col').map((c) => c.scrollTop);
        const open = $$('#main form[id]:not([hidden]), #main details[open]').map((f) => f.id || null);
        const filter = $('#boardfilter') ? formData2($('#boardfilter')) : null;
        for (const sel of ['#main', '.navlinks', '.switch-menu']) {
          const a = $(sel), b = doc.querySelector(sel);
          if (a && b) a.replaceWith(b);
        }
        const nb = $('#board');
        if (nb) { nb.scrollLeft = sx; $$('#board .col').forEach((c, i) => (c.scrollTop = cols[i] || 0)); }
        open.forEach((id) => { if (id) { const f = document.getElementById(id); if (f) f.hidden = false; } });
        if (filter) { const f = $('#boardfilter'); for (const k in filter) f.querySelector(`[name=${k}]`).value = filter[k]; }
        $('#live').hidden = true;
        afterRender();
      } finally {
        refreshing = null;
      }
    })();
    return refreshing;
  }
  function formData2(el) { const o = {}; $$('[name]', el).forEach((i) => (o[i.name] = i.value)); return o; }

  function busy() {
    if (dragging) return true;
    const a = document.activeElement;
    if (a && a.closest('#main') && /^(input|textarea|select)$/i.test(a.tagName) && !a.closest('#boardfilter')) return true;
    return $$('#main textarea, #main input[type=text], #main input:not([type])').some((i) => !i.closest('#boardfilter') && i.value !== i.defaultValue);
  }

  let liveTimer;
  function onChange() {
    clearTimeout(liveTimer);
    liveTimer = setTimeout(() => {
      if (busy()) $('#live').hidden = false;
      else refresh();
    }, 300);
  }
  // Live updates. All tabs share one stream through a SharedWorker; without
  // SharedWorker each tab streams only while visible and closes on navigation,
  // so streams never hog the browser's per-host connection limit.
  function connect() {
    const onEvent = (data) => {
      try {
        const e = JSON.parse(data);
        if (project() && e.project !== project()) return;
      } catch (_) { /* ignore malformed */ }
      onChange();
    };
    if (window.SharedWorker) {
      try {
        const w = new SharedWorker('/static/events-worker.js?v=' + document.body.dataset.asset, { name: 'tasker-events' });
        w.port.onmessage = (m) => onEvent(m.data);
        w.port.start();
        addEventListener('pagehide', () => w.port.postMessage('bye'));
        return;
      } catch (_) { /* fall through */ }
    }
    let es = null;
    const open = () => {
      if (es) return;
      es = new EventSource('/api/events' + (project() ? '?project=' + project() : ''));
      es.addEventListener('change', (ev) => onEvent(ev.data));
    };
    const close = () => { if (es) { es.close(); es = null; } };
    document.addEventListener('visibilitychange', () => {
      if (document.hidden) close();
      else { open(); onChange(); } // catch up on anything missed while hidden
    });
    addEventListener('pagehide', close);
    if (!document.hidden) open();
  }
  document.addEventListener('click', (e) => { if (e.target.closest('#live')) refresh(); });

  function afterRender() {
    applyFilter();
    syncSelection();
    if (window.mermaid && $('.mermaid')) {
      const dark = matchMedia('(prefers-color-scheme: dark)').matches;
      window.mermaid.initialize({ startOnLoad: false, theme: dark ? 'dark' : 'default', securityLevel: 'loose' });
      window.mermaid.run({ querySelector: '.mermaid' });
    }
  }

  afterRender();
  // Connect after the page (and its assets) finished loading.
  if (document.readyState === 'complete') connect();
  else addEventListener('load', connect, { once: true });
  // Restored from the back/forward cache: the old connection was released on pagehide.
  addEventListener('pageshow', (e) => { if (e.persisted) { connect(); refresh(); } });
})();
