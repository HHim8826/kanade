import { post } from './api.js';

// Passkeys (WebAuthn). The server's options and the device's answers carry binary data as
// base64url text.

const b64 = (buf) => {
  let s = '';
  for (const c of new Uint8Array(buf)) s += String.fromCharCode(c);
  return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
};
const unb64 = (s) => Uint8Array.from(atob(s.replace(/-/g, '+').replace(/_/g, '/') + '==='.slice((s.length + 3) % 4)), (c) => c.charCodeAt(0));

export const passkeysSupported = () => typeof window.PublicKeyCredential === 'function' && !!navigator.credentials;

// addPasskey makes a passkey for this account on this device, after the password is confirmed.
export async function addPasskey(password, name) {
  const o = await post('/passkeys/options', { password });
  const cred = await navigator.credentials.create({
    publicKey: {
      ...o, challenge: unb64(o.challenge), user: { ...o.user, id: unb64(o.user.id) },
      excludeCredentials: o.excludeCredentials.map((c) => ({ ...c, id: unb64(c.id) })),
    },
  });
  return post('/passkeys', {
    name, clientDataJSON: b64(cred.response.clientDataJSON), attestationObject: b64(cred.response.attestationObject),
  });
}

// loginWithPasskey lets the device offer the passkeys it has for this site, and logs in with the
// one chosen.
export async function loginWithPasskey() {
  const o = await post('/passkeys/login/options', {}, { allow401: true });
  const cred = await navigator.credentials.get({ publicKey: { ...o, challenge: unb64(o.challenge), allowCredentials: [] } });
  const r = cred.response;
  return post('/passkeys/login', {
    id: b64(cred.rawId), clientDataJSON: b64(r.clientDataJSON), authenticatorData: b64(r.authenticatorData),
    signature: b64(r.signature), userHandle: r.userHandle ? b64(r.userHandle) : '',
    device: 'web: ' + navigator.userAgent.slice(0, 240) + ' (passkey)', cookie: true,
  }, { allow401: true });
}

// passkeyMessage says what went wrong in plain words; null when the person just closed the prompt.
export function passkeyMessage(err) {
  switch (err && err.name) {
    case 'NotAllowedError':
    case 'AbortError':
      return null;
    case 'InvalidStateError':
      return '這個裝置已經有這個帳號的 passkey。';
    case 'SecurityError':
      return '這個網址不能使用 passkey（需要網站的 https 網址）。';
    case 'NotSupportedError':
      return '這個裝置不支援所需的 passkey 類型。';
  }
  if (err && err.status === 403) return '密碼錯誤。';
  if (err && err.status === 429) return '嘗試次數過多，請 15 分鐘後再試。';
  if (err && err.status === 401) return '無法用這個 passkey 登入：它可能已被移除，或屬於其他網站。';
  return (err && err.message) || '發生錯誤';
}
