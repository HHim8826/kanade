import { useEffect, useState } from '../../vendor/hooks.module.js';
import { api, get, post } from '../api.js';
import { checkPresence, device, deviceName } from '../presence.js';
import { keepInAddress, parseHash } from '../router.js';
import { ErrorBox, Icon, Spinner, html, toast } from '../ui.js';
import { confirmDialog } from './organize.js';

// Discord's status (review #135): the server shows what this account plays as its owner's Discord
// status, like Spotify's: the owner links their Discord account once (Discord's OAuth, with the
// presence permission of the Social SDK), and nothing runs on their computer or phone. Covers,
// audio addresses and logins never leave Kanade.

const results = {
  linked: ['已連結 Discord。開始播放後，你的 Discord 狀態會顯示正在聽的歌。', 'info'],
  denied: ['你在 Discord 取消了授權，沒有連結。', 'error'],
  expired: ['這次連結已過期，請再按一次「連結 Discord 帳號」。', 'error'],
  scope: ['Discord 沒有給「更新狀態」的權限：請確認應用程式已開啟 Social SDK，再連結一次。', 'error'],
  refused: ['Discord 拒絕了這次連結：請確認 Client ID、Client Secret 與 OAuth2 Redirects 的網址都正確。', 'error'],
  failed: ['連結失敗：Discord 沒有回應，請稍後再試。', 'error'],
};

const statuses = [['idle', '閒置'], ['online', '線上'], ['dnd', '請勿打擾']];

export function DiscordSettings() {
  const [info, setInfo] = useState(null);
  const [error, setError] = useState(null);
  const load = () => get('/discord', { timeout: 10000 }).then((r) => {
    setInfo(r);
    setError(null);
    checkPresence({ publish: !!(r.link && !r.link.error) });
  }, setError);
  useEffect(() => {
    load();
    // Back from Discord: say how it went, once.
    const r = parseHash().query.get('discord');
    if (r && results[r]) {
      toast(...results[r]);
      keepInAddress('settings');
    }
  }, []);
  // Linked: how it is doing, now and then.
  useEffect(() => {
    if (!info || !info.link) return;
    const t = setInterval(load, 15000);
    return () => clearInterval(t);
  }, [info && !!info.link]);
  const link = async () => {
    try {
      location.href = (await post('/discord/link')).url;
    } catch (e) {
      toast(e.message, 'error');
    }
  };
  const change = (patch) => api('PATCH', '/discord/link', patch).then(load, (e) => toast(e.message, 'error'));
  const unlink = () => confirmDialog({
    title: '解除 Discord 連結', action: '解除連結', danger: true,
    children: html`<p>Kanade 會停止更新你的 Discord 狀態，並向 Discord 撤銷這次授權。之後要再顯示，需要重新連結。</p>`,
    onConfirm: () => api('DELETE', '/discord/link').then(() => { checkPresence({ publish: false }); load(); }),
  });
  const l = info && info.link;
  const st = info && info.state;
  const ready = info && info.client_id && info.has_secret;
  // Browsers to follow: this one, any, the one followed now and those playing now.
  const browsers = new Map([[device, `這個瀏覽器（${deviceName}）`]]);
  for (const p of info ? info.players : []) if (!browsers.has(p.device)) browsers.set(p.device, p.device_name || '另一個瀏覽器');
  return html`<h2 class="section-title">Discord 狀態</h2>
    <div class="card pad discord-settings">
      <div class="sub">像 Spotify 一樣，在你的 Discord 個人狀態顯示 Kanade 正在播放的歌（「正在聽 Kanade」）。由伺服器回報，電腦和手機都不用另外安裝程式；只有在播放時才會連上 Discord。只會送出歌名、歌手、專輯與進度，不會傳出封面、音檔網址或登入資訊。</div>
      <${ErrorBox} error=${error} onRetry=${load} />
      ${!info && !error && html`<${Spinner} />`}
      ${info && html`<${AppForm} info=${info} onSaved=${load} />`}
      ${info && !l && html`<div class="actions">
        <button class="btn filled" disabled=${!ready} onClick=${link}><${Icon} name="link" />連結 Discord 帳號</button>
        ${!ready && html`<span class="sub">先填好上面的 Client ID 與 Client Secret。</span>`}
      </div>`}
      ${l && html`<div class="companion">
        <div class="toggle-row">
          <span class="grow"><span class="title">已連結 ${l.name}</span>
            <span class=${'sub' + (l.error ? ' state-failed' : st.connected ? ' state-completed' : '')}>${l.error ? l.error
              : st.connected ? `已連上 Discord${st.showing ? `，顯示「${st.showing}」` : ''}` : st.error ? `連線失敗，稍後重試：${st.error}` : '沒有在播放時不連線'}</span></span>
          ${l.error ? html`<button class="btn tonal" disabled=${!ready} onClick=${link}>重新連結</button>` : ''}
          <button class="btn text" onClick=${unlink}>解除連結</button>
        </div>
        <label class="field">顯示哪個瀏覽器播放的歌
          <select value=${l.follow} onChange=${(e) => change({ follow: e.target.value, follow_name: browsers.get(e.target.value) || '' })}>
            <option value="">任何裝置（最近開始播放的）</option>
            ${[...browsers].map(([d, name]) => html`<option key=${d} value=${d}>${name}</option>`)}
            ${l.follow && !browsers.has(l.follow) && html`<option value=${l.follow}>${l.follow_name || '另一個瀏覽器'}</option>`}
          </select></label>
        <div class="chips" role="group" aria-label="顯示的內容">
          <span class="chip-check on">歌名</span>
          ${[['artist', '歌手'], ['album', '專輯'], ['time', '播放進度']].map(([k, label]) => html`<label key=${k} class="chip-check">
            <input type="checkbox" checked=${l.show[k]} onChange=${(e) => change({ show: { ...l.show, [k]: e.target.checked } })} />${label}</label>`)}
          <label class="chip-check"><input type="checkbox" checked=${l.show.paused === 'show'}
            onChange=${(e) => change({ show: { ...l.show, paused: e.target.checked ? 'show' : 'clear' } })} />暫停時顯示「已暫停」</label>
        </div>
        <label class="field">專輯封面
          <select value=${l.show.cover || 'bangumi'} onChange=${(e) => change({ show: { ...l.show, cover: e.target.value } })}>
            <option value="bangumi">只用專輯綁定的 Bangumi 條目封面（公開圖片）</option>
            <option value="all">也用 Kanade 的封面（會產生公開的封面網址）</option>
            <option value="none">不顯示封面</option>
          </select></label>
        ${l.show.cover === 'all' && html`<p class="hint tight">沒有 Bangumi 條目的專輯，會用一個無法猜到、只有封面圖片的公開網址讓 Discord 抓取；這個網址會出現在 Discord 的狀態資料裡（看得到你的網域）。改回其他選項後網址即失效。</p>`}
        <label class="field">播放時你在 Discord 的線上狀態
          <select value=${l.status} onChange=${(e) => change({ status: e.target.value })}>
            ${statuses.map(([k, label]) => html`<option key=${k} value=${k}>${label}</option>`)}
          </select></label>
        <p class="hint tight">Kanade 播放時會以這個狀態連上 Discord；你在其他裝置上線時，Discord 會合併顯示。若你在 Discord 設成隱身，播放時好友仍可能看到你。</p>
      </div>`}
    </div>`;
}

// AppForm sets the Discord application the link is made with ("正在聽 <its name>").
function AppForm({ info, onSaved }) {
  const [id, setId] = useState(info.client_id || '');
  const [secret, setSecret] = useState('');
  const [image, setImage] = useState(info.image || '');
  const [busy, setBusy] = useState(false);
  const dirty = id !== (info.client_id || '') || secret !== '' || image !== (info.image || '');
  const save = async () => {
    setBusy(true);
    try {
      await api('PUT', '/discord/app', { client_id: id.trim(), client_secret: secret.trim(), image: image.trim() });
      setSecret('');
      toast('已儲存');
      onSaved();
    } catch (e) {
      toast(e.message, 'error');
    }
    setBusy(false);
  };
  const copy = () => navigator.clipboard.writeText(info.redirect_uri).then(() => toast('已複製'), () => toast('無法複製，請手動選取', 'error'));
  return html`<details class="app-setup" open=${!(info.client_id && info.has_secret)}>
    <summary>Discord 應用程式${info.client_id ? `：${info.client_id}` : '（還沒設定）'}</summary>
    <ol class="steps">
      <li>到 <a class="link" href="https://discord.com/developers/applications" target="_blank" rel="noopener noreferrer">Discord Developer Portal</a> 建立應用程式，名稱填「Kanade」（狀態會顯示「正在聽 Kanade」）；可以在 App Icon 上傳圖示。</li>
      <li>左側「Discord Social SDK」→「Getting Started」填表送出，開啟 Social SDK（這樣 Discord 才會給「更新狀態」的權限；不需要下載 SDK）。</li>
      <li>「OAuth2」→「Redirects」加入下面這個網址並儲存：
        <div class="command"><code>${info.redirect_uri}</code><button class="btn text" onClick=${copy}><${Icon} name="copy" />複製</button></div></li>
      <li>把同一頁的 Client ID 與 Client Secret 填在下面。Secret 只存在伺服器上，不會再顯示。</li>
    </ol>
    <div class="form-grid">
      <label class="field">Client ID<input value=${id} inputmode="numeric" placeholder="例如 1234567890123456789" onInput=${(e) => setId(e.target.value)} /></label>
      <label class="field">Client Secret<input type="password" value=${secret} autocomplete="off"
        placeholder=${info.has_secret ? '已設定（要更換才填）' : '從 OAuth2 頁複製'} onInput=${(e) => setSecret(e.target.value)} /></label>
      <label class="field span">圖示（選填）<input value=${image} placeholder="Rich Presence → Art Assets 上傳的圖片名稱，或 https 圖片網址"
        onInput=${(e) => setImage(e.target.value)} /></label>
    </div>
    <div class="actions"><button class="btn filled" disabled=${busy || !dirty} onClick=${save}>儲存</button></div>
  </details>`;
}
