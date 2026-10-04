import { useState } from '../../vendor/hooks.module.js';
import { BANDS, MAX_DB, PRESETS, effects, headroom, presetOf, setEffects } from '../effects.js';
import { useStore } from '../store.js';
import { Dialog, html, showDialog, toast } from '../ui.js';

// The sound effects' controls (review #139), in the settings and from now playing.

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
  const boost = headroom(e.gains);
  const choice = (key, label) => html`<button key=${key} type="button" class="choice" role="radio" aria-checked=${e.preset === key}
    disabled=${!e.eq} onClick=${() => pick(key)}>${label}</button>`;
  return html`<div class="effects">
    ${e.unsupported && html`<div class="error-box" role="alert"><span>這個瀏覽器無法處理音效；播放照常，不受影響。</span></div>`}
    ${e.suspended && html`<p class="hint">瀏覽器暫停了音效處理，點一下畫面或按任一鍵就會恢復。</p>`}
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
      <p class="hint tight">${boost > 0 ? `最大增強 +${boost} dB，已自動把整體降低 ${boost} dB，避免破音。` : '頻率（Hz）在下，增益（dB）在上。'}</p>
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
