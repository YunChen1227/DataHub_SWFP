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
}
