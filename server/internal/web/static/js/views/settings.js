import { get, post } from '../api.js';
import { ErrorBox, Icon, Spinner, fmtBytes, html, toast, useLoad } from '../ui.js';

export function Settings({ onLogout }) {
  const status = useLoad(() => get('/status'), []);
  const drive = useLoad(() => get('/drive'), []);
  const connect = async () => {
    try {
      const { url } = await post('/drive/auth', {});
      location.href = url; // Google returns to /oauth/google/callback on this server
    } catch (e) {
      toast(e.message, 'error');
    }
  };
  const logout = async () => {
    await post('/logout').catch(() => {});
    onLogout();
  };
  const d = drive.data;
  return html`<section>
    <h1 class="page-title">設定</h1>
    <h2 class="section-title">Google Drive</h2>
    <div class="card pad">
      ${drive.loading && html`<${Spinner} />`}
      <${ErrorBox} error=${drive.error} onRetry=${drive.reload} />
      ${d && (d.status.connected
        ? html`<div class="title">${d.account.email}</div>
            <div class="sub">已使用 ${fmtBytes(d.account.usage_bytes)}${d.account.limit_bytes ? ` / ${fmtBytes(d.account.limit_bytes)}` : ''}</div>
            ${d.status.testing_mode && html`<div class="task-error">OAuth 應用程式仍在「測試中」，授權 7 天後失效。</div>`}`
        : html`<div class="title">尚未連線</div>`)}
      <div class="actions"><button class="btn tonal" onClick=${connect}><${Icon} name="refresh" />${d && d.status.connected ? '重新連線' : '連線 Google Drive'}</button></div>
    </div>
    <h2 class="section-title">服務</h2>
    <div class="card pad">
      ${status.data && html`<div class="sub">下載器（aria2）：${status.data.aria2_ready ? '運作中' : '未就緒'}</div>
        <div class="sub">格式轉換與 CUE 分軌（FFmpeg）：${status.data.ffmpeg ? '可用' : '未安裝'}</div>
        <div class="sub">已運行 ${Math.floor(status.data.uptime_seconds / 3600)} 小時 ${Math.floor((status.data.uptime_seconds % 3600) / 60)} 分</div>`}
    </div>
    <h2 class="section-title">帳號</h2>
    <div class="actions"><button class="btn outlined" onClick=${logout}><${Icon} name="logout" />登出</button></div>
  </section>`;
}
