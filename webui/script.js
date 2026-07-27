const API = (typeof BASE_URL !== 'undefined' ? BASE_URL : '') + '/api/v1/entry';
let currentPage = 1;
let currentQuery = '';
let searchTimer = null;
let entriesRequestController = null;
let entriesRequestSequence = 0;
let pageCursors = new Map([[1, null]]);
const MIN_SEARCH_LENGTH = 3;
let deleteMode = false;

function showLogin() {
  document.getElementById('loginOverlay').style.display = 'flex';
  document.getElementById('mainApp').style.display = 'none';
}

function showApp() {
  document.getElementById('loginOverlay').style.display = 'none';
  document.getElementById('mainApp').style.display = 'block';
}

function setAuth(u, p, remember) {
  const val = btoa(u + ':' + p);
  if (remember) {
    localStorage.setItem('readsync_auth', val);
    sessionStorage.removeItem('readsync_auth');
  } else {
    sessionStorage.setItem('readsync_auth', val);
    localStorage.removeItem('readsync_auth');
  }
}

function getAuth() {
  return localStorage.getItem('readsync_auth') || sessionStorage.getItem('readsync_auth');
}

function clearAuth() {
  localStorage.removeItem('readsync_auth');
  sessionStorage.removeItem('readsync_auth');
}

function handleAuthError(err) {
  if (err?.message === 'Unauthorized') {
    clearAuth();
    showLogin();
    return true;
  }
  return false;
}

async function login() {
  let u = document.getElementById('loginUser').value.trim();
  let p = document.getElementById('loginPass').value;
  const remember = document.getElementById('rememberMe').checked;
  const err = document.getElementById('loginError');
  if (!u) {
    const colonIdx = p.indexOf(':');
    if (colonIdx !== -1) {
      u = p.substring(0, colonIdx);
      p = p.substring(colonIdx + 1);
      document.getElementById('loginUser').value = u;
      document.getElementById('loginPass').value = p;
    }
  }
  if (!u || !p) { err.textContent = '请输入用户名和密码'; return; }
  setAuth(u, p, remember);
  err.textContent = '';
  showApp();
  try {
    await loadEntries(1, '');
    pollLatestID();
    startPolling();
  } catch (e) {
    if (handleAuthError(e)) {
      err.textContent = '用户名或密码错误';
    } else {
      showLogin();
      err.textContent = '连接失败: ' + e.message;
    }
  }
}

// --- 用户脚本安装弹窗 ---
function getScriptInstallURL() {
  return (typeof BASE_URL !== 'undefined' ? BASE_URL : '') + '/userscript.user.js';
}

function openScriptInstall() {
  const scriptURL = getScriptInstallURL();
  document.getElementById('scriptUrlDisplay').textContent = window.location.origin + scriptURL;
  document.getElementById('scriptInstallOverlay').style.display = 'flex';
  document.getElementById('installStatusMsg').textContent = '';
  document.getElementById('installStatusMsg').className = 'install-status';
}

function closeScriptInstall() {
  document.getElementById('scriptInstallOverlay').style.display = 'none';
}

function setInstallStatus(text, isError) {
  const msg = document.getElementById('installStatusMsg');
  msg.textContent = text;
  msg.className = 'install-status' + (isError ? ' install-error' : ' install-success');
}

function directInstallScript() {
  const scriptURL = getScriptInstallURL();
  const tokenAPI = (typeof BASE_URL !== 'undefined' ? BASE_URL : '') + '/api/v1/userscript/token';
  setInstallStatus('正在获取安装令牌...', false);
  apiFetch(tokenAPI, { method: 'POST' }).then(function(data) {
    const installURL = window.location.origin + scriptURL + '?token=' + data.token;
    setInstallStatus('安装窗口已打开，请确认 Tampermonkey 的安装提示。', false);
    // 在新标签页打开 .user.js 链接，Tampermonkey 会自动检测并提示安装
    window.open(installURL, '_blank');
    closeScriptInstall();
  }).catch(function(err) {
    setInstallStatus('安装失败: ' + err.message, true);
  });
}

function copyScriptLink() {
  const scriptURL = window.location.origin + getScriptInstallURL();
  navigator.clipboard.writeText(scriptURL).then(function() {
    setInstallStatus('脚本链接已复制到剪贴板 ✓', false);
  }).catch(function() {
    // 回退方案：选中显示区域
    const display = document.getElementById('scriptUrlDisplay');
    const range = document.createRange();
    range.selectNodeContents(display);
    const selection = window.getSelection();
    selection.removeAllRanges();
    selection.addRange(range);
    setInstallStatus('请手动复制上方链接', false);
  });
}

// 点击遮罩层关闭弹窗
document.getElementById('scriptInstallOverlay').addEventListener('click', function(e) {
  if (e.target === this) closeScriptInstall();
});

// --- 事件监听初始化 ---
document.getElementById('loginBtn').addEventListener('click', login);
document.getElementById('loginPass').addEventListener('keydown', (e) => {
  if (e.key === 'Enter') login();
});
document.getElementById('installScriptBtn').addEventListener('click', openScriptInstall);
document.getElementById('directInstallBtn').addEventListener('click', directInstallScript);
document.getElementById('copyScriptLinkBtn').addEventListener('click', copyScriptLink);

async function apiFetch(url, opts = {}) {
  const headers = opts.headers || {};
  headers['Authorization'] = 'Basic ' + getAuth();
  if (opts.body && !(opts.body instanceof FormData)) {
    headers['Content-Type'] = 'application/json';
  }
  const res = await fetch(url, { ...opts, headers });
  if (res.status === 401) {
    throw new Error('Unauthorized');
  }
  if (!res.ok) {
    let msg = res.statusText;
    try { const j = await res.json(); msg = j.error || msg; } catch {}
    throw new Error(msg);
  }
  return res.json();
}

async function loadEntries(page, query) {
  const cursor = pageCursors.get(page);
  if (page > 1 && !cursor) return;

  if (entriesRequestController) entriesRequestController.abort();
  entriesRequestController = new AbortController();
  const requestSequence = ++entriesRequestSequence;
  const entriesElement = document.getElementById('entries');
  entriesElement.innerHTML = '<div class="loading">加载中...</div>';

  const parameters = new URLSearchParams({ per_page: '20' });
  if (query) parameters.set('q', query);
  if (cursor) {
    parameters.set('cursor_created_at', cursor.created_at);
    parameters.set('cursor_id', String(cursor.id));
  }

  try {
    const data = await apiFetch(`${API}?${parameters}`, { signal: entriesRequestController.signal });
    if (requestSequence !== entriesRequestSequence) return;

    if (!data.entries || data.entries.length === 0) {
      entriesElement.innerHTML = '<div class="empty">暂无记录</div>';
      renderPagination(page, false);
      return;
    }

    if (data.next_cursor) pageCursors.set(page + 1, data.next_cursor);
    else pageCursors.delete(page + 1);

    entriesElement.innerHTML = data.entries.map((entry, index) => {
      const entryNumber = (page - 1) * 20 + index + 1;
      const entryTime = new Date(entry.created_at).toLocaleString('zh-CN');
      return `<div class="entry">
        <input type="checkbox" class="delete-check" data-id="${entry.id}" style="display:${deleteMode ? 'inline-block' : 'none'}">
        <div class="num">${entryNumber}</div>
        <div class="content">
          <div class="title"><a href="${entry.url}" target="_blank">${escHtml(entry.title || entry.url)}</a></div>
          <div class="url">${escHtml(entry.url)}</div>
          <div class="time">${entryTime}</div>
        </div>
      </div>`;
    }).join('');

    renderPagination(page, data.has_more);
  } catch (error) {
    if (error.name === 'AbortError') return;
    throw error;
  }
}

function renderPagination(page, hasMore) {
  const el = document.getElementById('pagination');
  el.innerHTML = `
    <button onclick="goPage(${page - 1})" ${page <= 1 ? 'disabled' : ''}>上一页</button>
    <span>第 ${page} 页</span>
    <button onclick="goPage(${page + 1})" ${!hasMore ? 'disabled' : ''}>下一页</button>
  `;
}

function goPage(nextPage) {
  if (nextPage < 1 || (nextPage > 1 && !pageCursors.has(nextPage))) return;
  currentPage = nextPage;
  loadEntries(nextPage, currentQuery).catch(handleEntriesError);
}

function toggleDeleteMode() {
  deleteMode = !deleteMode;
  const btn = document.getElementById('deleteModeBtn');
  const checks = document.querySelectorAll('.delete-check');
  if (deleteMode) {
    btn.textContent = '确认删除';
    checks.forEach(c => c.style.display = 'inline-block');
  } else {
    btn.textContent = '删除';
    checks.forEach(c => { c.style.display = 'none'; c.checked = false; });
  }
}

async function confirmDelete() {
  const checks = document.querySelectorAll('.delete-check:checked');
  if (checks.length === 0) {
    deleteMode = false;
    const btn = document.getElementById('deleteModeBtn');
    btn.textContent = '删除';
    document.querySelectorAll('.delete-check').forEach(c => { c.style.display = 'none'; c.checked = false; });
    return;
  }
  const ids = Array.from(checks).map(c => Number(c.dataset.id));
  let failed = 0;
  for (const id of ids) {
    try {
      await apiFetch(`${API}/${id}`, { method: 'DELETE' });
    } catch {
      failed++;
    }
  }
  if (failed > 0) alert(`${failed} 条删除失败`);
  deleteMode = false;
  document.getElementById('deleteModeBtn').textContent = '删除';
  pageCursors = new Map([[1, null]]);
  currentPage = 1;
  loadEntries(1, currentQuery).catch(handleEntriesError);
  pollLatestID();
}

function escHtml(s) {
  const d = document.createElement('div');
  d.textContent = s;
  return d.innerHTML;
}

document.getElementById('searchInput').addEventListener('input', function() {
  clearTimeout(searchTimer);
  const nextQuery = this.value.trim();

  if (nextQuery === '') {
    applySearchQuery('');
    return;
  }
  if (Array.from(nextQuery).length < MIN_SEARCH_LENGTH) {
    currentQuery = nextQuery;
    currentPage = 1;
    pageCursors = new Map([[1, null]]);
    if (entriesRequestController) entriesRequestController.abort();
    document.getElementById('entries').innerHTML = `<div class="empty">请输入至少 ${MIN_SEARCH_LENGTH} 个字符</div>`;
    document.getElementById('pagination').innerHTML = '';
    return;
  }

  searchTimer = setTimeout(() => applySearchQuery(nextQuery), 300);
});

function applySearchQuery(nextQuery) {
  currentQuery = nextQuery;
  currentPage = 1;
  pageCursors = new Map([[1, null]]);
  loadEntries(1, currentQuery).catch(handleEntriesError);
}

function handleEntriesError(error) {
  if (error.name === 'AbortError' || handleAuthError(error)) return;
  document.getElementById('entries').innerHTML = `<div class="empty">加载失败：${escHtml(error.message)}</div>`;
}

// --- 轮询自动刷新 ---
let knownLatestID = null;
let pollTimer = null;
const POLL_INTERVAL_MS = (typeof POLL_INTERVAL !== 'undefined' ? POLL_INTERVAL : 30000);

async function pollLatestID() {
  const auth = getAuth();
  if (!auth) return;
  try {
    const res = await fetch(API + '/latest-id', {
      headers: { 'Authorization': 'Basic ' + auth }
    });
    if (!res.ok) return;
    const data = await res.json();
    const id = data.latest_id;
    if (knownLatestID !== null && id !== knownLatestID && currentQuery === '') {
      pageCursors = new Map([[1, null]]);
      currentPage = 1;
      loadEntries(1, '').catch(handleEntriesError);
    }
    knownLatestID = id;
  } catch (e) {
    // 网络错误静默忽略，下次再试
  }
}

function startPolling() {
  stopPolling();
  pollTimer = setInterval(pollLatestID, POLL_INTERVAL_MS);
}

function stopPolling() {
  if (pollTimer) {
    clearInterval(pollTimer);
    pollTimer = null;
  }
}

document.addEventListener('visibilitychange', () => {
  if (document.hidden) {
    stopPolling();
  } else {
    pollLatestID(); // 切回前台立刻检查一次
    startPolling();
  }
});

// --- 初始化 ---
const stored = getAuth();
if (stored) {
  showApp();
  loadEntries(1, '').then(() => {
    pollLatestID();
    startPolling();
  }).catch((e) => {
    handleAuthError(e);
  });
} else {
  showLogin();
}

window.addEventListener('unhandledrejection', (event) => {
  if (event.reason?.message === 'Unauthorized') {
    event.preventDefault();
    clearAuth();
    showLogin();
  }
});
