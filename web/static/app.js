(function () {
  const startEl = document.getElementById('start');
  const endEl = document.getElementById('end');
  const queryEl = document.getElementById('query');
  const btnEl = document.getElementById('searchBtn');
  const statusEl = document.getElementById('status');
  const resultsEl = document.getElementById('results');

  function todayStr() {
    return new Date().toISOString().slice(0, 10);
  }
  startEl.value = todayStr();
  endEl.value = todayStr();

  async function runSearch() {
    const q = queryEl.value.trim();
    if (!q) {
      statusEl.textContent = 'Type a keyword first.';
      return;
    }
    statusEl.textContent = 'Searching...';
    resultsEl.innerHTML = '';

    const params = new URLSearchParams({
      q: q,
      start: startEl.value,
      end: endEl.value,
    });

    try {
      const res = await fetch('/api/search?' + params.toString());
      const data = await res.json();
      if (!res.ok) {
        statusEl.textContent = 'Error: ' + (data.error || res.statusText);
        return;
      }
      statusEl.textContent = data.count + ' result(s) for "' + data.query + '" (' + data.start + ' to ' + data.end + ')';
      render(data.results || []);
    } catch (err) {
      statusEl.textContent = 'Request failed: ' + err;
    }
  }

  function render(results) {
    if (results.length === 0) {
      resultsEl.innerHTML = '<div class="empty">No matches.</div>';
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
      div.appendChild(snip);
      frag.appendChild(div);
    }
    resultsEl.innerHTML = '';
    resultsEl.appendChild(frag);
  }

  btnEl.addEventListener('click', runSearch);
  queryEl.addEventListener('keydown', function (e) {
    if (e.key === 'Enter') runSearch();
  });
})();
