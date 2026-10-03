// Thin client for /api/v1. The session lives in an HttpOnly cookie; every request carries the
// header the server requires from cookie-authenticated clients (CSRF protection).

export class ApiError extends Error {
  constructor(status, message, body, retryAfter = 0) {
    super(message);
    this.status = status;
    this.body = body;
    this.retryAfter = retryAfter; // seconds the server asked to wait before trying again
  }
}

// retryAfter reads Retry-After: seconds or a date.
function retryAfter(res) {
  const v = res.headers.get('Retry-After');
  if (!v) return 0;
  const n = /^\d+$/.test(v.trim()) ? Number(v) : (Date.parse(v) - Date.now()) / 1000;
  return Number.isFinite(n) && n > 0 ? Math.min(Math.ceil(n), 3600) : 0;
}

// unreadable says what went wrong when the answer is not Kanade's JSON: an error page from the proxy
// in front of the server (Cloudflare and the like), never shown as it is (review #79).
function unreadable(status) {
  if (status === 502 || status === 503 || status === 504 || (status >= 520 && status <= 530)) {
    return `暫時連不上伺服器（HTTP ${status}），可能正在重新啟動或網路不穩，請稍後再試。`;
  }
  return status >= 200 && status < 300 ? '伺服器的回應無法讀取，請稍後再試。' : `伺服器回應錯誤（HTTP ${status}），請稍後再試。`;
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
  // timeout (ms) gives up on a server that does not answer, for reads a page cannot show without.
  const signal = opts.timeout ? AbortSignal.any([AbortSignal.timeout(opts.timeout), ...(opts.signal ? [opts.signal] : [])]) : opts.signal;
  let res, text;
  try {
    res = await fetch('/api/v1' + path, { method, headers, body: payload, credentials: 'same-origin', signal, keepalive: !!opts.keepalive });
    text = await res.text();
  } catch (e) {
    throw e.name === 'TimeoutError' ? new ApiError(0, '伺服器沒有回應，請稍後再試') : e;
  }
  if (res.status === 401 && !opts.allow401) onUnauthorized();
  let data = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {
    throw new ApiError(res.status, unreadable(res.status), null, retryAfter(res));
  }
  if (!res.ok) throw new ApiError(res.status, (data && data.error) || unreadable(res.status), data, retryAfter(res));
  return data;
}

export const get = (path, opts) => api('GET', path, undefined, opts);
export const post = (path, body, opts) => api('POST', path, body, opts);

export const streamURL = (assetId) => `/api/v1/stream/${assetId}`;
export const coverURL = (coverId, size = 300) => (coverId ? `/api/v1/covers/${coverId}?size=${size}` : null);
