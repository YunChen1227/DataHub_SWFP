import React, { useEffect, useState } from 'react'
import { api } from '../api.js'

const emptyForm = { name: '', mobile: '' }

function maskMobile(m) {
  if (!m) return '-'
  const s = String(m)
  if (s.length <= 7) return s[0] + '*'.repeat(Math.max(s.length - 1, 0))
  return s.slice(0, 3) + '****' + s.slice(-4)
}

function fmtDate(v) {
  if (!v) return '-'
  const d = new Date(v)
  if (isNaN(d.getTime()) || d.getFullYear() < 1971) return '-'
  return d.toLocaleString()
}

export default function Users() {
  const [users, setUsers] = useState([])
  const [err, setErr] = useState('')
  const [form, setForm] = useState(emptyForm)
  const [secret, setSecret] = useState(null)
  const [editing, setEditing] = useState(null)
  const [query, setQuery] = useState('')

  const load = async (q) => {
    setErr('')
    try {
      const { users } = await api.listUsers(q !== undefined ? q : query)
      setUsers(users || [])
    } catch (e) {
      setErr(e.message)
    }
  }

  useEffect(() => {
    load('')
  }, [])

  const search = (e) => {
    e.preventDefault()
    load(query)
  }

  const resetSearch = () => {
    setQuery('')
    load('')
  }

  const create = async (e) => {
    e.preventDefault()
    setErr('')
    try {
      const res = await api.createUser({ name: form.name, mobile: form.mobile })
      setForm(emptyForm)
      setSecret({ appKey: res.user.appKey, secret: res.secret, title: '新用户已创建' })
      load()
    } catch (e) {
      setErr(e.message)
    }
  }

  const saveEdit = async () => {
    setErr('')
    try {
      await api.updateUser(editing.licenseId, {
        status: editing.status,
        mobile: editing.mobile || '',
      })
      setEditing(null)
      load()
    } catch (e) {
      setErr(e.message)
    }
  }

  const remove = async (u) => {
    if (!confirm(`确认删除用户 ${u.appKey}（${u.name || '-'}）？`)) return
    setErr('')
    try {
      await api.deleteUser(u.licenseId)
      load()
    } catch (e) {
      setErr(e.message)
    }
  }

  const rotate = async (u) => {
    if (!confirm(`确认为 ${u.appKey} 轮换 secret？旧 secret 立即失效。`)) return
    setErr('')
    try {
      const { secret } = await api.rotateSecret(u.licenseId)
      setSecret({ appKey: u.appKey, secret, title: 'secret 已轮换' })
      load()
    } catch (e) {
      setErr(e.message)
    }
  }

  return (
    <>
      <div className="card">
        <h2>新建用户（SWFP 路由）</h2>
        <p className="muted">此处创建的用户及其 license（appKey / secret）仅可调用 SWFP 路由。</p>
        <form className="form-grid" onSubmit={create}>
          <div>
            <label>名称/备注</label>
            <input value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} />
          </div>
          <div>
            <label>手机号</label>
            <input value={form.mobile} onChange={(e) => setForm({ ...form, mobile: e.target.value })} placeholder="13800001234" />
          </div>
          <div>
            <button className="btn" type="submit">创建并生成密钥</button>
          </div>
        </form>
      </div>

      {err && <div className="error">{err}</div>}

      <div className="card">
        <h2>SWFP 用户列表（{users.length}）</h2>
        <form className="toolbar" onSubmit={search}>
          <div>
            <label>检索（uuid / 名称 / 手机号）</label>
            <input value={query} onChange={(e) => setQuery(e.target.value)} placeholder="输入关键字" />
          </div>
          <div>
            <button className="btn" type="submit">查询</button>
          </div>
          <div>
            <button className="btn ghost" type="button" onClick={resetSearch}>重置</button>
          </div>
        </form>
        <div style={{ overflowX: 'auto' }}>
          <table>
            <thead>
              <tr>
                <th>uuid</th><th>名称</th><th>手机号</th><th>状态</th>
                <th>调用次数</th><th>成功查得数</th>
                <th>密钥创建时间</th><th>过期日期</th><th>创建时间</th><th>操作</th>
              </tr>
            </thead>
            <tbody>
              {users.map((u) => (
                <tr key={u.licenseId}>
                  <td><code>{u.appKey}</code></td>
                  <td>{u.name || '-'}</td>
                  <td>{maskMobile(u.mobile)}</td>
                  <td><span className={'badge ' + u.status}>{u.status}</span></td>
                  <td><strong>{u.totalCalls}</strong></td>
                  <td><strong>{u.serviceUsed}</strong></td>
                  <td className="muted">{fmtDate(u.secretCreatedAt)}</td>
                  <td className="muted">{fmtDate(u.validTo)}</td>
                  <td className="muted">{fmtDate(u.createdAt)}</td>
                  <td className="row-actions">
                    <button className="btn ghost small" onClick={() => setEditing({ ...u })}>编辑</button>
                    <button className="btn ghost small" onClick={() => rotate(u)}>轮换密钥</button>
                    <button className="btn danger small" onClick={() => remove(u)}>删除</button>
                  </td>
                </tr>
              ))}
              {users.length === 0 && (
                <tr><td colSpan="10" className="muted">暂无用户</td></tr>
              )}
            </tbody>
          </table>
        </div>
      </div>

      {editing && (
        <div className="modal-backdrop" onClick={() => setEditing(null)}>
          <div className="card modal" onClick={(e) => e.stopPropagation()}>
            <h2>编辑用户 — {editing.appKey}</h2>
            <div className="field">
              <label>状态</label>
              <select value={editing.status} onChange={(e) => setEditing({ ...editing, status: e.target.value })}>
                <option value="ACTIVE">ACTIVE</option>
                <option value="SUSPENDED">SUSPENDED</option>
                <option value="EXPIRED">EXPIRED</option>
              </select>
            </div>
            <div className="field">
              <label>手机号</label>
              <input value={editing.mobile || ''} onChange={(e) => setEditing({ ...editing, mobile: e.target.value })} placeholder="13800001234" />
            </div>
            <div className="field">
              <label>调用次数</label>
              <input type="number" value={editing.totalCalls} disabled readOnly />
            </div>
            <div className="field">
              <label>成功查得数</label>
              <input type="number" value={editing.serviceUsed} disabled readOnly />
            </div>
            <div className="row-actions" style={{ marginTop: 12 }}>
              <button className="btn" onClick={saveEdit}>保存</button>
              <button className="btn ghost" onClick={() => setEditing(null)}>取消</button>
            </div>
          </div>
        </div>
      )}

      {secret && (
        <div className="modal-backdrop" onClick={() => setSecret(null)}>
          <div className="card modal" onClick={(e) => e.stopPropagation()}>
            <h2>{secret.title}</h2>
            <p className="muted">uuid：<code>{secret.appKey}</code></p>
            <p className="muted">secret（仅此一次展示，请立即保存）：</p>
            <div className="secret-box">{secret.secret}</div>
            <button className="btn" onClick={() => setSecret(null)}>我已保存</button>
          </div>
        </div>
      )}
    </>
  )
}
