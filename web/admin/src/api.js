// Admin API client: login via /admin/api/login; data via /admin/api/swfp/...
const BASE = '/admin/api'

export const VERSIONS = ['swfp']

let token = localStorage.getItem('adminToken') || ''
let version = localStorage.getItem('adminVersion') || 'swfp'
if (!VERSIONS.includes(version)) version = 'swfp'

export function setToken(t) {
  token = t || ''
  if (token) localStorage.setItem('adminToken', token)
  else localStorage.removeItem('adminToken')
}

export function getToken() {
  return token
}

export function setVersion(v) {
  version = VERSIONS.includes(v) ? v : 'swfp'
  localStorage.setItem('adminVersion', version)
}

export function getVersion() {
  return version
}

// req issues a version-scoped data request (prefixes /{ver}).
async function req(method, path, body) {
  return rawReq(method, '/' + version + path, body)
}

// rawReq issues a request against the raw admin base (no version prefix),
// used for the shared control-plane login.
async function rawReq(method, path, body) {
  const headers = { 'Content-Type': 'application/json' }
  if (token) headers['Authorization'] = 'Bearer ' + token
  const res = await fetch(BASE + path, {
    method,
    headers,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  })
  const text = await res.text()
  const data = text ? JSON.parse(text) : {}
  if (!res.ok) {
    const err = new Error(data.error || 'HTTP ' + res.status)
    err.status = res.status
    throw err
  }
  return data
}

export const api = {
  login: (username, password) => rawReq('POST', '/login', { username, password }),
  listUsers: (q) => req('GET', '/users' + (q ? '?q=' + encodeURIComponent(q) : '')),
  createUser: (u) => req('POST', '/users', u),
  updateUser: (id, u) => req('PATCH', '/users/' + encodeURIComponent(id), u),
  deleteUser: (id) => req('DELETE', '/users/' + encodeURIComponent(id)),
  rotateSecret: (id) => req('POST', '/users/' + encodeURIComponent(id) + '/rotate-secret'),
  listAudits: (query) => req('GET', '/audits' + (query || '')),
  // 逐源明细：某次请求调了哪些源、各源的上游订单号/请求号、耗时与成本。
  listUpstreamCalls: (requestId) =>
    req('GET', '/audits/' + encodeURIComponent(requestId) + '/calls'),
  // 用量统计：按 (用户, 年/月/日时间桶) 的请求次数与成功次数。
  usageStats: (query) => req('GET', '/stats' + (query || '')),
  // 某用户某时间桶内的按源分布：这些请求是哪个源查成功的。
  sourceStats: (query) => req('GET', '/stats/sources' + (query || '')),
}

// timeRange 把「粒度 + 选中周期值」换算成 [from, to) 的 RFC3339 边界 (北京时间
// +08:00)。gran 为空或 value 为空返回空区间 (表示不限)。与后端按 Asia/Shanghai
// (+08:00) 分桶口径一致。
//
// value 既接受周期输入框的值，也直接接受统计结果里的 bucket 标签——两者格式相同
// (YYYY / YYYY-MM / YYYY-MM-DD)，故按源下钻可直接复用本函数算区间。
export function timeRange(gran, value) {
  if (!gran || !value) return { from: '', to: '' }
  const pad = (n) => String(n).padStart(2, '0')
  if (gran === 'year') {
    const y = parseInt(value, 10)
    if (!y) return { from: '', to: '' }
    return { from: `${y}-01-01T00:00:00+08:00`, to: `${y + 1}-01-01T00:00:00+08:00` }
  }
  if (gran === 'month') {
    const [ys, ms] = String(value).split('-')
    const y = parseInt(ys, 10)
    const m = parseInt(ms, 10)
    if (!y || !m) return { from: '', to: '' }
    const ny = m === 12 ? y + 1 : y
    const nm = m === 12 ? 1 : m + 1
    return { from: `${y}-${pad(m)}-01T00:00:00+08:00`, to: `${ny}-${pad(nm)}-01T00:00:00+08:00` }
  }
  // day: value 形如 '2026-09-20'
  const start = new Date(value + 'T00:00:00+08:00')
  if (isNaN(start.getTime())) return { from: '', to: '' }
  const next = new Date(start.getTime() + 86400000)
  // 取 next 在 +08:00 下的日历日期字符串。
  const nextDay = new Date(next.getTime() + 8 * 3600000).toISOString().slice(0, 10)
  return { from: `${value}T00:00:00+08:00`, to: `${nextDay}T00:00:00+08:00` }
}
