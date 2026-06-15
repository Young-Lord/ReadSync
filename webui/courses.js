const COURSE_API = (typeof BASE_URL !== 'undefined' ? BASE_URL : '') + '/api/v1/course';
let editingId = null;

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
  if (remember) { localStorage.setItem('readsync_auth', val); sessionStorage.removeItem('readsync_auth'); }
  else { sessionStorage.setItem('readsync_auth', val); localStorage.removeItem('readsync_auth'); }
}

function getAuth() { return localStorage.getItem('readsync_auth') || sessionStorage.getItem('readsync_auth'); }
function clearAuth() { localStorage.removeItem('readsync_auth'); sessionStorage.removeItem('readsync_auth'); }

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
    const idx = p.indexOf(':');
    if (idx !== -1) { u = p.substring(0, idx); p = p.substring(idx + 1); }
  }
  if (!u || !p) { err.textContent = '请输入用户名和密码'; return; }
  setAuth(u, p, remember);
  err.textContent = '';
  showApp();
  try { await loadCourses(); } catch (e) {
    if (handleAuthError(e)) {
      err.textContent = '用户名或密码错误';
    } else {
      showLogin();
      err.textContent = '连接失败: ' + e.message;
    }
  }
}

document.getElementById('loginBtn').addEventListener('click', login);
document.getElementById('loginPass').addEventListener('keydown', (e) => { if (e.key === 'Enter') login(); });

async function apiFetch(url, opts = {}) {
  const headers = opts.headers || {};
  headers['Authorization'] = 'Basic ' + getAuth();
  if (opts.body && !(opts.body instanceof FormData)) headers['Content-Type'] = 'application/json';
  const res = await fetch(url, { ...opts, headers });
  if (res.status === 401) throw new Error('Unauthorized');
  if (!res.ok) {
    let msg = res.statusText;
    try { const j = await res.json(); msg = j.error || msg; } catch {}
    throw new Error(msg);
  }
  return res.json();
}

function escHtml(s) {
  const d = document.createElement('div'); d.textContent = s; return d.innerHTML;
}

function escAttr(s) {
  return s.replace(/&/g, '&amp;').replace(/'/g, '&#39;').replace(/"/g, '&quot;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

function jumpUrl(shortId) {
  const base = typeof BASE_URL !== 'undefined' ? BASE_URL : '';
  return window.location.origin + base + '/course/jump/' + shortId;
}

async function loadCourses() {
  const el = document.getElementById('courseList');
  el.innerHTML = '<div class="empty">加载中...</div>';
  const courses = await apiFetch(COURSE_API);
  if (!courses || courses.length === 0) {
    el.innerHTML = '<div class="empty">暂无课程，点击「新增课程」添加</div>';
    return;
  }
  el.innerHTML = courses.map(c => {
    const time = c.updated_at ? new Date(c.updated_at).toLocaleString('zh-CN') : '';
    const jumpLink = jumpUrl(c.short_id);
    return `<div class="course-item" data-course='${escAttr(JSON.stringify(c))}'>
      <div class="course-info">
        <div class="course-name">${escHtml(c.name)}</div>
        <div class="course-short-id">${escHtml(c.short_id)}</div>
        <div class="course-patterns">
          ${c.title_pattern ? '<span>标题: <code>' + escHtml(c.title_pattern) + '</code></span>' : ''}
          ${c.url_pattern ? '<span>URL: <code>' + escHtml(c.url_pattern) + '</code></span>' : ''}
          ${!c.title_pattern && !c.url_pattern ? '<span style="color:#999">未设置匹配规则</span>' : ''}
        </div>
        <div class="course-progress">
          ${c.latest_url
            ? '<div class="progress-url"><a href="' + escAttr(c.latest_url) + '" target="_blank">' + escHtml(c.latest_title || c.latest_url) + '</a></div>'
              + (time ? '<div class="progress-time">更新于 ' + time + '</div>' : '')
            : '<div class="progress-time">暂无阅读进度</div>'}
        </div>
        <div class="jump-link">快速跳转: <a href="${escAttr(jumpLink)}" target="_blank">${escHtml(jumpLink)}</a></div>
      </div>
      <div class="course-actions">
        <button class="primary edit-btn">编辑</button>
        <button class="danger delete-btn">删除</button>
      </div>
    </div>`;
  }).join('');

  el.querySelectorAll('.edit-btn').forEach(btn => {
    btn.addEventListener('click', () => {
      const c = JSON.parse(btn.closest('.course-item').dataset.course);
      editCourse(c.id, c.name, c.short_id, c.title_pattern, c.url_pattern);
    });
  });
  el.querySelectorAll('.delete-btn').forEach(btn => {
    btn.addEventListener('click', () => {
      const c = JSON.parse(btn.closest('.course-item').dataset.course);
      deleteCourse(c.id, c.name);
    });
  });
}

document.getElementById('addCourseBtn').addEventListener('click', () => {
  editingId = null;
  document.getElementById('modalTitle').textContent = '新增课程';
  document.getElementById('courseName').value = '';
  document.getElementById('courseShortId').value = '';
  document.getElementById('courseTitlePattern').value = '';
  document.getElementById('courseUrlPattern').value = '';
  document.getElementById('courseShortId').disabled = false;
  document.getElementById('modalError').textContent = '';
  document.getElementById('courseModal').style.display = 'flex';
});

function editCourse(id, name, shortId, titlePattern, urlPattern) {
  editingId = id;
  document.getElementById('modalTitle').textContent = '编辑课程';
  document.getElementById('courseName').value = name;
  document.getElementById('courseShortId').value = shortId;
  document.getElementById('courseTitlePattern').value = titlePattern;
  document.getElementById('courseUrlPattern').value = urlPattern;
  document.getElementById('courseShortId').disabled = false;
  document.getElementById('modalError').textContent = '';
  document.getElementById('courseModal').style.display = 'flex';
}

document.getElementById('modalCancel').addEventListener('click', () => {
  document.getElementById('courseModal').style.display = 'none';
});

document.getElementById('modalSave').addEventListener('click', async () => {
  const name = document.getElementById('courseName').value.trim();
  const shortId = document.getElementById('courseShortId').value.trim();
  const titlePattern = document.getElementById('courseTitlePattern').value.trim();
  const urlPattern = document.getElementById('courseUrlPattern').value.trim();
  const errEl = document.getElementById('modalError');

  if (!name || !shortId) { errEl.textContent = '课程名称和 short_id 为必填'; return; }

  try {
    if (editingId) {
      await apiFetch(`${COURSE_API}/${editingId}`, {
        method: 'PUT',
        body: JSON.stringify({ name, short_id: shortId, title_pattern: titlePattern, url_pattern: urlPattern }),
      });
    } else {
      await apiFetch(COURSE_API, {
        method: 'POST',
        body: JSON.stringify({ name, short_id: shortId, title_pattern: titlePattern, url_pattern: urlPattern }),
      });
    }
    document.getElementById('courseModal').style.display = 'none';
    await loadCourses();
  } catch (e) {
    errEl.textContent = e.message;
  }
});

async function deleteCourse(id, name) {
  if (!confirm('确定删除课程「' + name + '」？')) return;
  try {
    await apiFetch(`${COURSE_API}/${id}`, { method: 'DELETE' });
    await loadCourses();
  } catch (e) {
    alert('删除失败: ' + e.message);
  }
}

const stored = getAuth();
if (stored) {
  showApp();
  loadCourses().catch((e) => { handleAuthError(e); });
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
