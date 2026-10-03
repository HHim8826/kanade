import { useState } from '../../vendor/hooks.module.js';
import { getInitial, post } from '../api.js';
import { loginWithPasskey, passkeyMessage, passkeysSupported } from '../passkey.js';
import { ErrorBox, Icon, Spinner, html, useLoad } from '../ui.js';

export function Login({ onLogin }) {
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);
  // Resolve the available methods before showing the form, rather than moving it
  // when the passkey button arrives. A failed or stalled check still permits passwords.
  const methods = useLoad(() => passkeysSupported()
    ? getInitial('/passkeys/available', { allow401: true, timeoutMs: 3000 })
    : Promise.resolve({ available: false }), []);
  const passkeys = methods.data && methods.data.available;
  const withPasskey = async () => {
    setBusy(true);
    setError(null);
    try {
      await loginWithPasskey();
      onLogin();
    } catch (err) {
      const msg = passkeyMessage(err);
      if (msg) setError(new Error(msg));
      setBusy(false);
    }
  };
  const submit = async (e) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await post('/login', { username, password, device: 'web: ' + navigator.userAgent.slice(0, 80), cookie: true }, { allow401: true });
      onLogin();
    } catch (err) {
      setError(err.status === 401 ? new Error('帳號或密碼錯誤') : err.status === 429 ? new Error('嘗試次數過多，請 15 分鐘後再試') : err);
      setBusy(false);
    }
  };
  if (methods.loading) return html`<main class="login"><${Spinner} /></main>`;
  return html`<main class="login">
    <form class="card pad login-card" onSubmit=${submit}>
      <div class="brand"><${Icon} name="library" size=${40} /><h1>Kanade</h1></div>
      <label class="field"><span>帳號</span><input autocomplete="username" value=${username} onInput=${(e) => setUsername(e.target.value)} required /></label>
      <label class="field"><span>密碼</span><input type="password" autocomplete="current-password" value=${password} onInput=${(e) => setPassword(e.target.value)} required /></label>
      <${ErrorBox} error=${error} />
      <button class="btn filled wide" disabled=${busy}>${busy ? '登入中…' : '登入'}</button>
      ${passkeys && html`<div class="or"><span>或</span></div>
        <button type="button" class="btn outlined wide" disabled=${busy} onClick=${withPasskey}><${Icon} name="passkey" />使用 passkey 登入</button>`}
    </form>
  </main>`;
}
