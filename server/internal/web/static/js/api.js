// Thin client for /api/v1. The session lives in an HttpOnly cookie; every request carries the
// header the server requires from cookie-authenticated clients (CSRF protection).

export class ApiError extends Error {
  constructor(status, message, body) {
    super(message);
    this.status = status;
    this.body = body;
  }
}

let onUnauthorized = () => {};
export function setUnauthorizedHandler(f) {
  onUnauthorized = f;
}

export async function api(method, path, body, opts = {}) {
  const headers = { 'X-Requested-With': 'kanade' };
  let payload;
  if (body instanceof Blob || body instanceof ArrayBuffer) {
    payload = body;
    if (opts.contentType) headers['Content-Type'] = opts.contentType;
  } else if (body !== undefined) {
    headers['Content-Type'] = 'application/json';
    payload = JSON.stringify(body);
  }
  const res = await fetch('/api/v1' + path, {
    method, headers, body: payload, credentials: 'same-origin', signal: opts.signal, keepalive: !!opts.keepalive,
  });
  const text = await res.text();
  let data = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {
    data = { error: text.slice(0, 200) };
  }
  if (res.status === 401 && !opts.allow401) onUnauthorized();
  if (!res.ok) throw new ApiError(res.status, (data && data.error) || res.statusText, data);
  return data;
}

export const get = (path, opts) => api('GET', path, undefined, opts);
export const post = (path, body, opts) => api('POST', path, body, opts);

export const streamURL = (assetId) => `/api/v1/stream/${assetId}`;
export const coverURL = (coverId, size = 300) => (coverId ? `/api/v1/covers/${coverId}?size=${size}` : null);
