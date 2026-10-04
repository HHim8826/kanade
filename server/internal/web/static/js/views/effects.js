import { useEffect, useState } from '../../vendor/hooks.module.js';
import { get, post } from '../api.js';
import { BANDS, MAX_DB, PRESETS, TARGETS, boost as boostOf, effects, presetOf, setEffects } from '../effects.js';
import { useStore } from '../store.js';
import { Dialog, ErrorBox, html, showDialog, toast } from '../ui.js';

// The sound effects' controls (reviews #136, #139), in the settings and from now playing.

const fmtHz = (f) => (f >= 1000 ? `${f / 1000}k` : String(f));
const fmtDb = (g) => (g > 0 ? `+${g}` : String(g));
// fill colors the track from 0 dB, its middle, to the value.
const fill = (g) => {
  const at = ((g + MAX_DB) / (2 * MAX_DB)) * 100;
  return { '--lo': `${Math.min(50, at)}%`, '--hi': `${Math.max(50, at)}%` };
};

export function showEffects() {
  showDialog((close) => html`<${Dialog} title="音效" onClose=${close} actions=${html`<button class="btn filled" onClick=${close}>完成</button>`}>
    <${EffectsPanel} />
  <//>`);
}

export function EffectsPanel() {
  const e = useStore(effects);
  const [naming, setNaming] = useState(null); // the name a curve is being saved under
  const pick = (key) => {
    const built = PRESETS.find(([k]) => k === key);
    const gains = built ? built[2].slice() : e.customs[key.slice('custom:'.length)].slice();
    setEffects({ preset: key, gains });
  };
  const setBand = (i, v) => {
    const gains = e.gains.slice();
    gains[i] = v;
    setEffects({ gains, preset: presetOf(gains, e.customs) });
  };
  const save = (ev) => {
    ev.preventDefault();
    const name = naming.trim();
    if (!name) return;
    setEffects({ customs: { ...e.customs, [name]: e.gains.slice() }, preset: 'custom:' + name });
    setNaming(null);
    toast(`已儲存「${name}」`);
  };
  const remove = (name) => {
    const customs = { ...e.customs };
    delete customs[name];
    setEffects({ customs, preset: presetOf(e.gains, customs) });
  };
  const custom = e.preset.startsWith('custom:') ? e.preset.slice('custom:'.length) : '';
  const boost = boostOf(e.gains);
  const choice = (key, label) => html`<button key=${key} type="button" class="choice" role="radio" aria-checked=${e.preset === key}
    disabled=${!e.eq} onClick=${() => pick(key)}>${label}</button>`;
  const pickChoice = (label, list, value, set) => html`<div class="sub">${label}</div>
    <div class="choices" role="radiogroup" aria-label=${label}>
      ${list.map(([k, text]) => html`<button key=${k} type="button" class="choice" role="radio" aria-checked=${value === k} onClick=${() => set(k)}>${text}</button>`)}
    </div>`;
  return html`<div class="effects">
    ${e.unsupported && html`<div class="error-box" role="alert"><span>這個瀏覽器無法處理音效；播放照常，不受影響。</span></div>`}
    ${e.suspended && html`<p class="hint">瀏覽器暫停了音效處理，點一下畫面或按任一鍵就會恢復。</p>`}
    ${pickChoice('音量平衡', [['off', '關閉'], ['track', '曲目平衡'], ['album', '專輯平衡']], e.balance, (k) => setEffects({ balance: k }))}
    ${e.balance !== 'off' && pickChoice('目標響度', TARGETS, e.target, (k) => setEffects({ target: k }))}
    <p class="hint tight">曲目平衡把每首歌調到差不多響，適合跨專輯隨機播放；專輯平衡讓同一張專輯用同一個音量，保留曲目之間原本的強弱。只在播放時調整，不改動音檔。</p>
    ${e.balance !== 'off' && !e.eq && html`<p class="hint tight">沒有開均衡器時只能把較大聲的歌調小，比目標小聲的歌維持原本的音量；開了均衡器（平直也可以）才會一併調大。</p>`}
    <${Measured} />
    <hr class="divider" />
    <label class="toggle-row">
      <span class="grow"><span class="title">均衡器</span>
        <span class="sub">10 段，各 ±${MAX_DB} dB。只在這台裝置播放時處理，不改動音檔。</span></span>
      <input type="checkbox" role="switch" checked=${e.eq} disabled=${e.unsupported} onChange=${(ev) => setEffects({ eq: ev.target.checked })} />
    </label>
    <div class=${'eq' + (e.eq ? '' : ' off')}>
      <div class="choices" role="radiogroup" aria-label="預設曲線">
        ${PRESETS.map(([k, label]) => choice(k, label))}
        ${Object.keys(e.customs).map((n) => choice('custom:' + n, n))}
        ${e.preset === '' && html`<span class="choice ghost" aria-current="true">自訂（未儲存）</span>`}
      </div>
      <div class="eq-bands">
        ${BANDS.map((f, i) => html`<label key=${f} class="eq-band">
          <span class="eq-val">${fmtDb(e.gains[i])}</span>
          <input type="range" min=${-MAX_DB} max=${MAX_DB} step="0.5" value=${e.gains[i]} disabled=${!e.eq} style=${fill(e.gains[i])}
            aria-label=${`${fmtHz(f)}Hz`} aria-valuetext=${`${fmtDb(e.gains[i])} dB`} onInput=${(ev) => setBand(i, parseFloat(ev.target.value))} />
          <span class="eq-hz">${fmtHz(f)}</span>
        </label>`)}
      </div>
      <p class="hint tight">${boost > 0 ? `曲線最多增強 ${boost} dB（相鄰頻段會疊加），已把整體降低同樣多，避免破音。` : '頻率（Hz）在下，增益（dB）在上。'}</p>
      ${naming === null
        ? html`<div class="actions">
            <button class="btn text" disabled=${!e.eq || e.gains.every((g) => g === 0)} onClick=${() => setEffects({ gains: PRESETS[0][2].slice(), preset: 'flat' })}>歸零</button>
            <button class="btn text" disabled=${!e.eq} onClick=${() => setNaming(custom)}>另存為預設…</button>
            ${custom && html`<button class="btn text danger-text" disabled=${!e.eq} onClick=${() => remove(custom)}>刪除「${custom}」</button>`}
          </div>`
        : html`<form class="actions eq-save" onSubmit=${save}>
            <input aria-label="預設名稱" placeholder="預設名稱" value=${naming} maxLength="20" onInput=${(ev) => setNaming(ev.target.value)} autoFocus />
            <button class="btn tonal" type="submit" disabled=${!naming.trim()}>儲存</button>
            <button class="btn text" type="button" onClick=${() => setNaming(null)}>取消</button>
          </form>`}
    </div>
  </div>`;
}

// Measured is how much of the library the server has measured for the balance, with the scan that
// measures the rest (it starts by itself; it can be stopped, or run again for the files that failed).
function Measured() {
  const [st, setSt] = useState(null);
  const [error, setError] = useState(null);
  const load = () => get('/loudness/scan').then((v) => { setSt(v); setError(null); }, setError);
  useEffect(() => {
    load();
    const t = setInterval(() => document.visibilityState === 'visible' && load(), st && st.scan.running ? 3000 : 30000);
    return () => clearInterval(t);
  }, [st && st.scan.running]);
  const act = (body) => post('/loudness/scan', body).then((v) => setSt(v), (err) => toast(err.message, 'error'));
  if (error) return html`<${ErrorBox} error=${error} onRetry=${load} />`;
  if (!st) return null;
  const { status: s, scan } = st;
  if (!st.available) return html`<p class="hint">伺服器沒有安裝 FFmpeg，無法分析歌曲的響度；裝好之後就會開始。</p>`;
  const left = s.files - s.measured - s.failed;
  return html`<div class="measured">
    <span class="sub">已分析 ${s.measured}／${s.files} 首${scan.running ? `，正在分析（這次 ${scan.done} 首）…` : left > 0 ? `，還有 ${left} 首` : ''}${s.failed ? `；${s.failed} 首無法分析` : ''}</span>
    ${scan.running
      ? html`<button class="btn text" onClick=${() => act({ run: false })}>停止</button>`
      : html`${left > 0 && html`<button class="btn text" onClick=${() => act({ run: true })}>繼續分析</button>`}
          ${s.failed > 0 && html`<button class="btn text" onClick=${() => act({ run: true, failed: true })}>重試無法分析的</button>`}`}
    ${scan.error && html`<p class="hint tight">${scan.error === 'some files could not be read from Drive; they are measured on another scan'
      ? '有些檔案這次沒能從 Drive 讀完，下次分析時會再試。' : scan.error}</p>`}
  </div>`;
}
