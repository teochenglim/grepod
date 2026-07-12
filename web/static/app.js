(function () {
  // ---- Search view elements ----
  const startEl = document.getElementById('start');
  const endEl = document.getElementById('end');
  const queryEl = document.getElementById('query');
  const btnEl = document.getElementById('searchBtn');
  const statusEl = document.getElementById('status');
  const resultsEl = document.getElementById('results');
  const groupToggleEl = document.getElementById('groupToggle');
  const levelTabsEl = document.getElementById('levelTabs');
  const loadMoreBtn = document.getElementById('loadMoreBtn');

  // ---- Tail view elements ----
  const modeSearchBtn = document.getElementById('modeSearchBtn');
  const modeTailBtn = document.getElementById('modeTailBtn');
  const searchView = document.getElementById('searchView');
  const tailView = document.getElementById('tailView');
  const tailPodEl = document.getElementById('tailPod');
  const tailContainerEl = document.getElementById('tailContainer');
  const tailQueryEl = document.getElementById('tailQuery');
  const tailStartBtn = document.getElementById('tailStartBtn');
  const tailStopBtn = document.getElementById('tailStopBtn');
  const tailStatusEl = document.getElementById('tailStatus');
  const tailResultsEl = document.getElementById('tailResults');
  const tailResumeBtn = document.getElementById('tailResumeBtn');

  function todayStr() {
    return new Date().toISOString().slice(0, 10);
  }
  function daysAgoStr(n) {
    const d = new Date();
    d.setDate(d.getDate() - n);
    return d.toISOString().slice(0, 10);
  }
  // Matches the server's own default search window (see
  // internal/api/handler.go's defaultSearchWindowDays) so the UI's
  // initial state isn't narrower than what a bare /api/search?q= call
  // already returns.
  startEl.value = daysAgoStr(6);
  endEl.value = todayStr();

  // ===================== Mode toggle =====================

  function setMode(mode) {
    const isSearch = mode === 'search';
    searchView.hidden = !isSearch;
    tailView.hidden = isSearch;
    modeSearchBtn.classList.toggle('active', isSearch);
    modeTailBtn.classList.toggle('active', !isSearch);
    if (isSearch) {
      stopTail();
    }
  }
  modeSearchBtn.addEventListener('click', function () { setMode('search'); });
  modeTailBtn.addEventListener('click', function () { setMode('tail'); });

  // ===================== Known pod/container filters =====================

  function populateSelect(selectEl, values, placeholder) {
    const current = selectEl.value;
    selectEl.innerHTML = '';
    const allOpt = document.createElement('option');
    allOpt.value = '';
    allOpt.textContent = placeholder;
    selectEl.appendChild(allOpt);
    for (const v of values) {
      const opt = document.createElement('option');
      opt.value = v;
      opt.textContent = v;
      selectEl.appendChild(opt);
    }
    if (values.includes(current)) selectEl.value = current;
  }

  async function loadKnownFilters() {
    try {
      const res = await fetch('/api/known?days=7');
      if (!res.ok) return;
      const data = await res.json();
      populateSelect(tailPodEl, data.pods || [], 'All pods');
      populateSelect(tailContainerEl, data.containers || [], 'All containers');
    } catch (err) {
      // Non-fatal: the dropdowns just stay empty; free-text filtering via
      // /api/tail's own params still works without this.
    }
  }
  loadKnownFilters();

  // ===================== Search =====================

  // Accumulates every page loaded for the current query/filters so
  // "Group occurrences" can aggregate across all of them, not just the
  // most recently fetched page.
  let allResults = [];
  let nextCursor = '';
  let currentLevel = '';

  function runSearch() {
    const q = queryEl.value.trim();
    if (!q) {
      statusEl.textContent = 'Type a keyword first.';
      return;
    }
    allResults = [];
    nextCursor = '';
    fetchPage(true);
  }

  async function fetchPage(isFirstPage) {
    const q = queryEl.value.trim();
    if (!q) return;

    statusEl.textContent = isFirstPage ? 'Searching...' : 'Loading more...';
    loadMoreBtn.hidden = true;

    const params = new URLSearchParams({
      q: q,
      start: startEl.value,
      end: endEl.value,
      level: currentLevel,
    });
    if (!isFirstPage && nextCursor) params.set('cursor', nextCursor);

    try {
      const res = await fetch('/api/search?' + params.toString());
      const data = await res.json();
      if (!res.ok) {
        statusEl.textContent = 'Error: ' + (data.error || res.statusText);
        return;
      }
      allResults = isFirstPage ? (data.results || []) : allResults.concat(data.results || []);
      nextCursor = data.next_cursor || '';
      statusEl.textContent = allResults.length + ' result(s) for "' + data.query + '" (' + data.start + ' to ' + data.end + ')' + (nextCursor ? ', more available' : '');
      renderSearchResults();
      loadMoreBtn.hidden = !nextCursor;
    } catch (err) {
      statusEl.textContent = 'Request failed: ' + err;
    }
  }

  // normalizeForGrouping strips the server's <mark> highlighting and
  // obvious variable tokens (timestamps, UUIDs, bare numbers) from a
  // snippet so that e.g. "retrying request id=abc123 after 4 attempts"
  // and "retrying request id=def456 after 9 attempts" collapse into the
  // same group instead of each getting their own one-off entry.
  function normalizeForGrouping(snippetHtml) {
    return snippetHtml
      .replace(/<\/?mark>/g, '')
      .replace(/\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b/g, '<uuid>')
      .replace(/\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})?/g, '<ts>')
      .replace(/\b\d+\b/g, '<n>')
      .trim();
  }

  function groupResults(results) {
    const groups = new Map();
    for (const r of results) {
      const key = r.pod + '|' + r.container + '|' + normalizeForGrouping(r.snippet);
      const g = groups.get(key);
      if (!g || r.timestamp > g.timestamp) {
        groups.set(key, { pod: r.pod, container: r.container, snippet: r.snippet, timestamp: r.timestamp, count: (g ? g.count : 0) + 1 });
      } else {
        g.count += 1;
      }
    }
    return Array.from(groups.values()).sort(function (a, b) { return b.timestamp < a.timestamp ? -1 : b.timestamp > a.timestamp ? 1 : 0; });
  }

  function renderSearchResults() {
    const items = groupToggleEl.checked ? groupResults(allResults) : allResults;
    render(resultsEl, items, groupToggleEl.checked);
  }

  function render(container, results, grouped) {
    if (results.length === 0) {
      container.innerHTML = '<div class="empty">No matches.</div>';
      return;
    }
    const frag = document.createDocumentFragment();
    for (const r of results) {
      const div = document.createElement('div');
      div.className = 'line';
      const prefix = document.createElement('span');
      prefix.className = 'prefix';
      prefix.textContent = '[' + r.pod + '/' + r.container + ']';
      const ts = document.createElement('span');
      ts.className = 'ts';
      ts.textContent = r.timestamp;
      const snip = document.createElement('span');
      // snippet already contains <mark> tags from the server for highlighting
      snip.innerHTML = r.snippet;
      div.appendChild(prefix);
      div.appendChild(ts);
      if (grouped && r.count > 1) {
        const count = document.createElement('span');
        count.className = 'count';
        count.textContent = '×' + r.count;
        div.appendChild(count);
      }
      div.appendChild(snip);
      frag.appendChild(div);
    }
    container.innerHTML = '';
    container.appendChild(frag);
  }

  btnEl.addEventListener('click', runSearch);
  queryEl.addEventListener('keydown', function (e) {
    if (e.key === 'Enter') runSearch();
  });
  groupToggleEl.addEventListener('change', renderSearchResults);
  loadMoreBtn.addEventListener('click', function () { fetchPage(false); });

  levelTabsEl.addEventListener('click', function (e) {
    const btn = e.target.closest('.level-tab');
    if (!btn) return;
    for (const el of levelTabsEl.querySelectorAll('.level-tab')) el.classList.remove('active');
    btn.classList.add('active');
    currentLevel = btn.dataset.level;
    if (queryEl.value.trim()) runSearch();
  });

  // ===================== Tail =====================

  let tailSource = null;
  let tailAutoScroll = true;
  const TAIL_MAX_LINES = 2000;

  function stopTail() {
    if (tailSource) {
      tailSource.close();
      tailSource = null;
    }
    tailStartBtn.hidden = false;
    tailStopBtn.hidden = true;
    tailStatusEl.textContent = 'Not connected.';
  }

  function startTail() {
    stopTail();
    tailResultsEl.innerHTML = '';
    tailAutoScroll = true;
    tailResumeBtn.hidden = true;

    const params = new URLSearchParams();
    if (tailPodEl.value) params.set('pod', tailPodEl.value);
    if (tailContainerEl.value) params.set('container', tailContainerEl.value);
    if (tailQueryEl.value.trim()) params.set('q', tailQueryEl.value.trim());

    tailSource = new EventSource('/api/tail?' + params.toString());
    tailStatusEl.textContent = 'Connecting...';
    tailStartBtn.hidden = true;
    tailStopBtn.hidden = false;

    tailSource.onopen = function () {
      tailStatusEl.textContent = 'Live.';
    };
    tailSource.onerror = function () {
      tailStatusEl.textContent = 'Disconnected (retrying...).';
    };
    tailSource.onmessage = function (e) {
      let ev;
      try {
        ev = JSON.parse(e.data);
      } catch (err) {
        return;
      }
      appendTailLine(ev);
    };
  }

  function appendTailLine(ev) {
    const div = document.createElement('div');
    div.className = 'line';
    const prefix = document.createElement('span');
    prefix.className = 'prefix';
    prefix.textContent = '[' + ev.pod + '/' + ev.container + ']';
    const ts = document.createElement('span');
    ts.className = 'ts';
    ts.textContent = ev.timestamp;
    const content = document.createElement('span');
    content.textContent = ev.content;
    div.appendChild(prefix);
    div.appendChild(ts);
    div.appendChild(content);

    if (tailResultsEl.querySelector('.empty')) tailResultsEl.innerHTML = '';
    // Pausing (auto-scroll off) must never drop lines — keep appending,
    // just stop forcing the scroll position while the user reads back.
    tailResultsEl.appendChild(div);
    while (tailResultsEl.children.length > TAIL_MAX_LINES) {
      tailResultsEl.removeChild(tailResultsEl.firstChild);
    }

    if (tailAutoScroll) {
      tailResultsEl.scrollTop = tailResultsEl.scrollHeight;
    } else {
      tailResumeBtn.hidden = false;
    }
  }

  tailResultsEl.addEventListener('scroll', function () {
    const atBottom = tailResultsEl.scrollHeight - tailResultsEl.scrollTop - tailResultsEl.clientHeight < 30;
    if (atBottom) {
      tailAutoScroll = true;
      tailResumeBtn.hidden = true;
    } else {
      tailAutoScroll = false;
    }
  });
  tailResumeBtn.addEventListener('click', function () {
    tailAutoScroll = true;
    tailResumeBtn.hidden = true;
    tailResultsEl.scrollTop = tailResultsEl.scrollHeight;
  });

  tailStartBtn.addEventListener('click', startTail);
  tailStopBtn.addEventListener('click', stopTail);
  tailQueryEl.addEventListener('keydown', function (e) {
    if (e.key === 'Enter') startTail();
  });
})();
