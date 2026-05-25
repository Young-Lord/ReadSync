const API = (typeof BASE_URL !== 'undefined' ? BASE_URL : '') + '/api/v1/entry';
let currentPage = 1;
let currentQuery = '';
let searchTimer = null;

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
    clearAuth();
    showLogin();
    err.textContent = e.message === 'Unauthorized' ? '用户名或密码错误' : '连接失败: ' + e.message;
  }
}

document.getElementById('loginBtn').addEventListener('click', login);
document.getElementById('loginPass').addEventListener('keydown', (e) => {
  if (e.key === 'Enter') login();
});

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
  const el = document.getElementById('entries');
  el.innerHTML = '<div class="loading">加载中...</div>';

  let url = `${API}?page=${page}&per_page=20`;
  if (query) url += '&q=' + encodeURIComponent(query);

  const data = await apiFetch(url);
  if (!data.entries || data.entries.length === 0) {
    el.innerHTML = '<div class="empty">暂无记录</div>';
    document.getElementById('pagination').innerHTML = '';
    return;
  }

  el.innerHTML = data.entries.map((e, i) => {
    const n = (page - 1) * 20 + i + 1;
    const time = new Date(e.created_at).toLocaleString('zh-CN');
    return `<div class="entry">
      <div class="num">${n}</div>
      <div class="content">
        <div class="title"><a href="${e.url}" target="_blank">${escHtml(e.title || e.url)}</a></div>
        <div class="url">${escHtml(e.url)}</div>
        <div class="time">${time}</div>
      </div>
      <div class="actions"><button class="danger" onclick="deleteEntry(${e.id})">删除</button></div>
    </div>`;
  }).join('');

  renderPagination(data.page, data.has_more);
}

function renderPagination(page, hasMore) {
  const el = document.getElementById('pagination');
  el.innerHTML = `
    <button onclick="goPage(${page - 1})" ${page <= 1 ? 'disabled' : ''}>上一页</button>
    <span>第 ${page} 页</span>
    <button onclick="goPage(${page + 1})" ${!hasMore ? 'disabled' : ''}>下一页</button>
  `;
}

function goPage(p) {
  if (p < 1) return;
  currentPage = p;
  loadEntries(p, currentQuery).catch(() => {});
}

async function deleteEntry(id) {
  if (!confirm('确定删除此记录？')) return;
  try {
    await apiFetch(`${API}/${id}`, { method: 'DELETE' });
  } catch (e) {
    alert('删除失败: ' + e.message);
    return;
  }
  loadEntries(currentPage, currentQuery);
  pollLatestID(); // 同步最新 ID，避免轮询误判
}

function escHtml(s) {
  const d = document.createElement('div');
  d.textContent = s;
  return d.innerHTML;
}

document.getElementById('searchInput').addEventListener('input', function() {
  clearTimeout(searchTimer);
  searchTimer = setTimeout(() => {
    currentQuery = this.value.trim();
    currentPage = 1;
    loadEntries(currentPage, currentQuery);
  }, 300);
});

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
    if (knownLatestID !== null && id !== knownLatestID) {
      loadEntries(currentPage, currentQuery);
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
  }).catch(() => {
    clearAuth();
    showLogin();
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
