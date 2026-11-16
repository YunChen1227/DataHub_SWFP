import React, { useEffect, useState } from 'react'
import { api } from '../api.js'

const PROVIDER_LABEL = {
  entcredit: '证通',
  ctax: '惠众',
  sy: '税友',
  salesdata: '凯盈云',
}

export default function UpstreamAllowlist() {
  const [view, setView] = useState(null)
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')

  const load = async () => {
    setLoading(true)
    setError('')
    try {
      setView(await api.getUpstreamAllowlist())
    } catch (e) {
      setError(e.message || String(e))
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    load()
  }, [])

  const toggle = async () => {
    if (!view || saving) return
    const next = !view.enabled
    const tip = next
      ? '开启后：仅允许白名单中的税号，并只调用对应上游的票/税维度；不在名单或维度不符将直接拒绝。确认开启？'
      : '关闭后恢复现网正常寻源（仍保留税友 CSV 闸门）。确认关闭？'
    if (!window.confirm(tip)) return
    setSaving(true)
    setError('')
    try {
      setView(await api.setUpstreamAllowlistEnabled(next))
    } catch (e) {
      setError(e.message || String(e))
    } finally {
      setSaving(false)
    }
  }

  const stats = view?.stats || {}
  const by = stats.byProvider || {}

  return (
    <div className="card">
      <h2>上游白名单</h2>
      <p className="muted" style={{ marginTop: 0 }}>
        开启后只允许访问白名单中的税号，并仅调用对应上游的对应维度（票 / 税 / 票税）；
        关闭则与现网一致。名单来自对账单种子（证通 / 税友 / 惠众 / 凯盈云）与税友 CSV。
      </p>

      {error && <div className="error">{error}</div>}
      {loading && <div className="muted">加载中…</div>}

      {!loading && view && (
        <>
          <div className="toolbar" style={{ alignItems: 'center' }}>
            <div>
              <label>运行时开关</label>
              <div style={{ display: 'flex', gap: 10, alignItems: 'center' }}>
                <span className={'badge ' + (view.enabled ? 'ACTIVE' : 'SUSPENDED')}>
                  {view.enabled ? '已开启（仅白名单）' : '已关闭（正常寻源）'}
                </span>
                <button className="btn" onClick={toggle} disabled={saving}>
                  {saving ? '保存中…' : view.enabled ? '关闭白名单限制' : '开启白名单限制'}
                </button>
              </div>
            </div>
          </div>

          <table>
            <thead>
              <tr>
                <th>统计项</th>
                <th>数量</th>
              </tr>
            </thead>
            <tbody>
              <tr>
                <td>条目总数（税号×上游）</td>
                <td>{stats.total ?? 0}</td>
              </tr>
              <tr>
                <td>不重复税号</td>
                <td>{stats.uniqueCodes ?? 0}</td>
              </tr>
              <tr>
                <td>仅税 / 仅票 / 票税</td>
                <td>
                  {stats.taxOnly ?? 0} / {stats.invoiceOnly ?? 0} / {stats.both ?? 0}
                </td>
              </tr>
              {Object.keys(PROVIDER_LABEL).map((p) => (
                <tr key={p}>
                  <td>{PROVIDER_LABEL[p]}（{p}）</td>
                  <td>{by[p] ?? 0}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      )}
    </div>
  )
}
