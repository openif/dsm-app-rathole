document.addEventListener('DOMContentLoaded', () => {
  // Elements
  const statusBadge = document.getElementById('statusBadge');
  const statusText = document.getElementById('statusText');
  const uptimeText = document.getElementById('uptimeText');

  const btnStart = document.getElementById('btnStart');
  const btnStop = document.getElementById('btnStop');
  const btnRestart = document.getElementById('btnRestart');

  const tabBtns = document.querySelectorAll('.tab-btn');
  const tabPanes = document.querySelectorAll('.tab-pane');

  const configEditor = document.getElementById('configEditor');
  const lineNumbers = document.getElementById('lineNumbers');
  const editorStats = document.getElementById('editorStats');
  const dirtyBadge = document.getElementById('dirtyBadge');
  const btnCopyConfig = document.getElementById('btnCopyConfig');
  const btnReloadConfig = document.getElementById('btnReloadConfig');
  const btnResetDefault = document.getElementById('btnResetDefault');
  const btnSaveConfig = document.getElementById('btnSaveConfig');

  const logContainer = document.getElementById('logContainer');
  const logViewer = document.getElementById('logViewer');
  const autoRefreshLog = document.getElementById('autoRefreshLog');
  const logSearchInput = document.getElementById('logSearchInput');
  const btnClearSearch = document.getElementById('btnClearSearch');
  const logFilterStats = document.getElementById('logFilterStats');
  const btnScrollTop = document.getElementById('btnScrollTop');
  const btnScrollBottom = document.getElementById('btnScrollBottom');
  const btnRefreshLog = document.getElementById('btnRefreshLog');
  const btnClearLog = document.getElementById('btnClearLog');
  const logMeta = document.getElementById('logMeta');

  const toast = document.getElementById('toast');

  let activeTab = 'configTab';
  let isRunning = false;
  let logInterval = null;
  let toastTimer = null;

  let originalConfig = '';
  let rawLogContent = '';
  let logFilterKeyword = '';

  // Get Synology CSRF token if available in parent DSM frame or cookies
  function getSynoToken() {
    try {
      if (window.parent && window.parent.SYNO && window.parent.SYNO.secToken) {
        return window.parent.SYNO.secToken;
      }
      if (window.parent && window.parent.SynoToken) {
        return window.parent.SynoToken;
      }
      if (window.SynoToken) {
        return window.SynoToken;
      }
    } catch (e) {}
    const match = document.cookie.match(/(?:^|;)\s*SynoToken\s*=\s*([^;]+)/);
    if (match) return match[1];
    return '';
  }

  // Smart API URL resolution (DSM WebMan vs standalone :8520)
  function getApiUrl(action, query = '') {
    const isWebman = window.location.pathname.includes('/webman/3rdparty/');
    let url = isWebman ? `api.cgi?action=${action}` : `/api/${action}`;
    const token = getSynoToken();
    const params = [];
    if (query) params.push(query);
    if (token && isWebman) params.push(`SynoToken=${encodeURIComponent(token)}`);
    if (params.length > 0) {
      url += (url.includes('?') ? '&' : '?') + params.join('&');
    }
    return url;
  }

  // Default template for reset
  const CLIENT_TEMPLATE = `# Rathole 客户端配置示例 (Noise 安全加密模式)
[client]
remote_addr = "你的公网服务器IP:37000"
default_token = "你的安全连接Token"
heartbeat_timeout = 40
retry_interval = 1

[client.transport]
type = "noise"

[client.transport.noise]
pattern = "Noise_NK_25519_ChaChaPoly_BLAKE2s"
remote_public_key = "服务端生成的PublicKey公钥"

# 服务 1: 映射群晖 DSM 网页管理端口 (默认 5000)
[client.services.dsm]
type = "tcp"
local_addr = "127.0.0.1:5000"
nodelay = true

# 服务 2: 映射群晖 SSH 终端端口 (默认 22)
[client.services.ssh]
type = "tcp"
local_addr = "127.0.0.1:22"
nodelay = true
`;

  // Interactive Toast notification with optional action button
  function showToast(msg, type = 'success', action = null) {
    if (toastTimer) {
      clearTimeout(toastTimer);
      toastTimer = null;
    }
    toast.innerHTML = '';

    const textSpan = document.createElement('span');
    textSpan.textContent = msg;
    toast.appendChild(textSpan);

    if (action && action.text && typeof action.onClick === 'function') {
      const actionBtn = document.createElement('button');
      actionBtn.className = 'toast-action-btn';
      actionBtn.textContent = action.text;
      actionBtn.addEventListener('click', (e) => {
        e.stopPropagation();
        action.onClick();
        toast.className = 'toast';
      });
      toast.appendChild(actionBtn);
    }

    toast.className = `toast show toast-${type}`;
    const duration = action ? 5000 : 2800;
    toastTimer = setTimeout(() => {
      toast.className = 'toast';
    }, duration);
  }

  // Tab switching
  tabBtns.forEach(btn => {
    btn.addEventListener('click', () => {
      const tabId = btn.getAttribute('data-tab');
      tabBtns.forEach(b => b.classList.remove('active'));
      tabPanes.forEach(p => p.classList.remove('active'));

      btn.classList.add('active');
      const pane = document.getElementById(tabId);
      if (pane) pane.classList.add('active');
      activeTab = tabId;

      if (activeTab === 'logTab') {
        fetchLog();
      } else if (activeTab === 'configTab') {
        updateLineNumbers();
        updateDirtyState();
      }
    });
  });

  // Editor Line Numbers & Dirty State Sync
  function updateLineNumbers() {
    const lines = configEditor.value.split('\n').length;
    let numbersHtml = '';
    for (let i = 1; i <= lines; i++) {
      numbersHtml += `<div>${i}</div>`;
    }
    lineNumbers.innerHTML = numbersHtml;
    editorStats.textContent = `${lines} 行 | ${configEditor.value.length} 字符`;
  }

  function updateDirtyState() {
    const isDirty = configEditor.value !== originalConfig;
    if (dirtyBadge) {
      dirtyBadge.style.display = isDirty ? 'inline-flex' : 'none';
    }
  }

  configEditor.addEventListener('input', () => {
    updateLineNumbers();
    updateDirtyState();
  });

  configEditor.addEventListener('scroll', () => {
    lineNumbers.scrollTop = configEditor.scrollTop;
  });

  // Tab indentation in textarea
  configEditor.addEventListener('keydown', e => {
    if (e.key === 'Tab') {
      e.preventDefault();
      const start = configEditor.selectionStart;
      const end = configEditor.selectionEnd;
      const val = configEditor.value;

      configEditor.value = val.substring(0, start) + '  ' + val.substring(end);
      configEditor.selectionStart = configEditor.selectionEnd = start + 2;
      updateLineNumbers();
      updateDirtyState();
    }
    // Ctrl + S or Cmd + S -> Save
    if ((e.ctrlKey || e.metaKey) && e.key === 's') {
      e.preventDefault();
      handleSaveConfig();
    }
  });

  // Window unload guard if config is dirty
  window.addEventListener('beforeunload', e => {
    if (configEditor.value !== originalConfig) {
      e.preventDefault();
      e.returnValue = '';
    }
  });

  // Fetch Status
  async function fetchStatus() {
    try {
      const res = await fetch(getApiUrl('status'));
      const data = await res.json();
      isRunning = !!data.running;

      if (isRunning) {
        statusBadge.className = 'status-badge running';
        statusText.textContent = '运行中';
        if (uptimeText) uptimeText.textContent = `运行时长: ${data.uptime || '-'}`;
        btnStart.disabled = true;
        btnStop.disabled = false;
        btnRestart.disabled = false;
      } else {
        statusBadge.className = 'status-badge stopped';
        statusText.textContent = data.message || '已停止';
        if (uptimeText) uptimeText.textContent = '运行时长: -';
        btnStart.disabled = false;
        btnStop.disabled = true;
        btnRestart.disabled = true;

        if (data.last_error) {
          statusText.textContent = '异常停止';
          statusText.title = data.last_error;
        }
      }

      if (data.config_path) {
        const pathEl = document.querySelector('.file-path');
        if (pathEl) pathEl.textContent = data.config_path;
      }

      if (data.log_size !== undefined) {
        const kb = (data.log_size / 1024).toFixed(1);
        logMeta.textContent = `日志大小: ${kb} KB`;
      }
    } catch (err) {
      statusBadge.className = 'status-badge stopped';
      statusText.textContent = '无法连接服务';
    }
  }

  // Load Config
  async function loadConfig() {
    try {
      const res = await fetch(getApiUrl('config'));
      const data = await res.json();
      if (data.success) {
        if (data.content && data.content.trim()) {
          configEditor.value = data.content;
        } else {
          configEditor.value = CLIENT_TEMPLATE;
        }
        originalConfig = configEditor.value;
        updateLineNumbers();
        updateDirtyState();
      } else {
        showToast(data.message || '读取配置失败', 'error');
      }
    } catch (err) {
      showToast('无法连接后端 API', 'error');
    }
  }

  // Save Config
  async function saveConfig() {
    try {
      const headers = { 'Content-Type': 'application/json' };
      const token = getSynoToken();
      if (token) headers['X-SYNO-TOKEN'] = token;

      const res = await fetch(getApiUrl('config'), {
        method: 'POST',
        headers: headers,
        body: JSON.stringify({ content: configEditor.value })
      });

      if (!res.ok) {
        let errDetail = `HTTP ${res.status}`;
        try {
          const errJson = await res.json();
          if (errJson.message) errDetail = errJson.message;
        } catch (_) {}
        showToast(`保存失败: ${errDetail}`, 'error');
        return false;
      }

      const data = await res.json();
      if (data.success) {
        originalConfig = configEditor.value;
        updateDirtyState();
        return true;
      } else {
        showToast(`保存失败: ${data.message || '未知错误'}`, 'error');
        return false;
      }
    } catch (err) {
      showToast(`保存失败: ${err.message || '网络连接异常'}`, 'error');
      return false;
    }
  }

  // Trigger Action
  async function triggerAction(action) {
    try {
      const headers = { 'Content-Type': 'application/json' };
      const token = getSynoToken();
      if (token) headers['X-SYNO-TOKEN'] = token;

      const res = await fetch(getApiUrl('action'), {
        method: 'POST',
        headers: headers,
        body: JSON.stringify({ action })
      });

      if (!res.ok) {
        let errDetail = `HTTP ${res.status}`;
        try {
          const errJson = await res.json();
          if (errJson.message) errDetail = errJson.message;
        } catch (_) {}
        showToast(`操作失败: ${errDetail}`, 'error');
        return;
      }

      const data = await res.json();
      if (data.success) {
        showToast(data.message || '操作成功');
        await fetchStatus();
        if (activeTab === 'logTab') fetchLog();
      } else {
        showToast(`操作失败: ${data.message}`, 'error');
      }
    } catch (err) {
      showToast('网络错误，无法发送指令', 'error');
    }
  }

  // Save Config handler with instant restart prompt action
  async function handleSaveConfig() {
    if (btnSaveConfig) btnSaveConfig.disabled = true;
    const ok = await saveConfig();
    if (ok) {
      // Provide interactive toast with one-click restart button
      showToast('配置文件已保存！', 'success', {
        text: '立即重启生效',
        onClick: () => {
          triggerAction('restart');
        }
      });
      // Gently pulse restart button to guide user
      if (btnRestart) {
        btnRestart.classList.add('btn-pulse');
        setTimeout(() => btnRestart.classList.remove('btn-pulse'), 5000);
      }
    }
    if (btnSaveConfig) btnSaveConfig.disabled = false;
  }

  if (btnSaveConfig) {
    btnSaveConfig.addEventListener('click', handleSaveConfig);
  }

  // Copy Config content
  if (btnCopyConfig) {
    btnCopyConfig.addEventListener('click', async () => {
      try {
        if (navigator.clipboard && navigator.clipboard.writeText) {
          await navigator.clipboard.writeText(configEditor.value);
        } else {
          configEditor.select();
          document.execCommand('copy');
        }
        showToast('配置文件已复制到剪贴板！');
      } catch (e) {
        showToast('复制失败，请手动选中复制', 'error');
      }
    });
  }

  btnStart.addEventListener('click', () => triggerAction('start'));
  btnStop.addEventListener('click', () => triggerAction('stop'));
  btnRestart.addEventListener('click', () => triggerAction('restart'));

  btnReloadConfig.addEventListener('click', () => {
    if (configEditor.value !== originalConfig) {
      if (!confirm('当前有未保存的修改，重新加载将丢失所有未保存的内容。确认重新加载？')) {
        return;
      }
    }
    loadConfig();
    showToast('已从磁盘重新加载配置');
  });

  if (btnResetDefault) {
    btnResetDefault.addEventListener('click', () => {
      if (!confirm('确认将当前配置恢复为默认客户端模板吗？未保存的修改将会丢失。')) {
        return;
      }
      configEditor.value = CLIENT_TEMPLATE;
      updateLineNumbers();
      updateDirtyState();
      showToast('已恢复为默认客户端模板，请点击“保存配置”保存！');
    });
  }

  // =========================================================================
  // Log Rendering, Syntax Highlighting & Search Filtering
  // =========================================================================

  function escapeHtml(str) {
    return str
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;');
  }

  function renderLog() {
    if (!rawLogContent) {
      logViewer.innerHTML = '<div class="log-line log-info">(暂无运行日志)</div>';
      logFilterStats.textContent = '';
      return;
    }

    const lines = rawLogContent.split('\n');
    let matchedLines = [];
    const keyword = logFilterKeyword.trim().toLowerCase();

    if (keyword) {
      matchedLines = lines.filter(line => line.toLowerCase().includes(keyword));
      logFilterStats.textContent = `匹配 ${matchedLines.length} / 共 ${lines.length} 行`;
      if (btnClearSearch) btnClearSearch.style.display = 'block';
    } else {
      matchedLines = lines;
      logFilterStats.textContent = `共 ${lines.length} 行`;
      if (btnClearSearch) btnClearSearch.style.display = 'none';
    }

    const isNearBottom = logContainer.scrollHeight - logContainer.clientHeight <= logContainer.scrollTop + 60;

    let html = '';
    for (let i = 0; i < matchedLines.length; i++) {
      const rawLine = matchedLines[i];
      if (!rawLine.trim()) continue;

      let lineClass = 'log-line';
      if (/ERROR|FATAL|Failed|Authentication failed/i.test(rawLine)) {
        lineClass += ' log-error';
      } else if (/WARN|Warning|Retry in/i.test(rawLine)) {
        lineClass += ' log-warn';
      } else {
        lineClass += ' log-info';
      }

      let escaped = escapeHtml(rawLine);

      // Highlight timestamps (e.g. 2026-09-12T10:24:22.012286Z)
      escaped = escaped.replace(/^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z)/, '<span class="log-time">$1</span>');

      // Highlight service tags (e.g. handle{service=dsm})
      escaped = escaped.replace(/(handle\{service=([^}]+)\})/g, '<span class="log-service">$1</span>');

      // Highlight keyword match if searching
      if (keyword) {
        const reg = new RegExp(`(${keyword.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')})`, 'gi');
        escaped = escaped.replace(reg, '<mark class="log-match">$1</mark>');
      }

      html += `<div class="${lineClass}">${escaped}</div>`;
    }

    logViewer.innerHTML = html || '<div class="log-line log-info">(未检索到符合条件的日志行)</div>';

    // Auto-scroll to bottom if user hasn't scrolled up
    if (isNearBottom || !keyword) {
      logContainer.scrollTop = logContainer.scrollHeight;
    }
  }

  // Fetch Log
  async function fetchLog() {
    try {
      const res = await fetch(getApiUrl('log', 'lines=500'));
      const data = await res.json();
      if (data.success) {
        rawLogContent = data.log || '';
        renderLog();
      }
    } catch (err) {
      logViewer.innerHTML = '<div class="log-line log-error">获取日志失败，网络异常</div>';
    }
  }

  // Search input listeners
  if (logSearchInput) {
    logSearchInput.addEventListener('input', (e) => {
      logFilterKeyword = e.target.value;
      renderLog();
    });
  }

  if (btnClearSearch) {
    btnClearSearch.addEventListener('click', () => {
      logSearchInput.value = '';
      logFilterKeyword = '';
      renderLog();
    });
  }

  // Scroll buttons
  if (btnScrollTop) {
    btnScrollTop.addEventListener('click', () => {
      logContainer.scrollTop = 0;
    });
  }

  if (btnScrollBottom) {
    btnScrollBottom.addEventListener('click', () => {
      logContainer.scrollTop = logContainer.scrollHeight;
    });
  }

  btnRefreshLog.addEventListener('click', fetchLog);
  btnClearLog.addEventListener('click', async () => {
    if (!confirm('确认清空当前日志文件？')) return;
    try {
      const headers = { 'Content-Type': 'application/json' };
      const token = getSynoToken();
      if (token) headers['X-SYNO-TOKEN'] = token;
      const res = await fetch(getApiUrl('clear_log'), {
        method: 'POST',
        headers: headers,
        body: JSON.stringify({})
      });
      if (!res.ok) {
        let errDetail = `HTTP ${res.status}`;
        try {
          const errJson = await res.json();
          if (errJson.message) errDetail = errJson.message;
        } catch (_) {}
        showToast(`清空日志失败: ${errDetail}`, 'error');
        return;
      }
      const data = await res.json();
      if (data.success) {
        showToast('日志已清空');
        fetchLog();
      } else {
        showToast(`清空日志失败: ${data.message || '未知原因'}`, 'error');
      }
    } catch (err) {
      showToast(`清空日志失败: ${err.message || '网络异常'}`, 'error');
    }
  });

  // Sample configuration copy buttons in Help tab
  document.querySelectorAll('.btn-copy-sample').forEach(btn => {
    btn.addEventListener('click', async () => {
      const targetId = btn.getAttribute('data-target');
      const el = document.getElementById(targetId);
      if (!el) return;
      try {
        if (navigator.clipboard && navigator.clipboard.writeText) {
          await navigator.clipboard.writeText(el.textContent);
        } else {
          const area = document.createElement('textarea');
          area.value = el.textContent;
          document.body.appendChild(area);
          area.select();
          document.execCommand('copy');
          document.body.removeChild(area);
        }
        showToast('配置示例已成功复制到剪贴板！');
      } catch (e) {
        showToast('复制失败，请手动选中复制', 'error');
      }
    });
  });

  // Polling timers
  setInterval(fetchStatus, 3000);
  logInterval = setInterval(() => {
    if (activeTab === 'logTab' && autoRefreshLog.checked) {
      fetchLog();
    }
  }, 2000);

  // Initial load
  fetchStatus();
  loadConfig();
});
