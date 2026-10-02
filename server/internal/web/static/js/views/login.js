import { useState } from '../../vendor/hooks.module.js';
import { post } from '../api.js';
import { ErrorBox, Icon, html } from '../ui.js';

export function Login({ onLogin }) {
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);
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
  return html`<main class="login">
    <form class="card pad login-card" onSubmit=${submit}>
      <div class="brand"><${Icon} name="library" size=${40} /><h1>Kanade</h1></div>
      <label class="field"><span>帳號</span><input autocomplete="username" value=${username} onInput=${(e) => setUsername(e.target.value)} required /></label>
      <label class="field"><span>密碼</span><input type="password" autocomplete="current-password" value=${password} onInput=${(e) => setPassword(e.target.value)} required /></label>
      <${ErrorBox} error=${error} />
      <button class="btn filled wide" disabled=${busy}>${busy ? '登入中…' : '登入'}</button>
    </form>
  </main>`;
}
