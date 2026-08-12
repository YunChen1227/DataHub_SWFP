import React, { useEffect, useState } from 'react'
import { api } from '../api.js'

export default function Audits() {
  const [rows, setRows] = useState([])
  const [err, setErr] = useState('')
  const [keyword, setKeyword] = useState('')
  const [busiCode, setBusiCode] = useState('')
  const [loading, setLoading] = useState(false)

  const load = async () => {
    setErr('')
    setLoading(true)
    try {
      const params = new URLSearchParams()
      if (keyword) params.set('q', keyword)
      if (busiCode) params.set('busiCode', busiCode)
      params.set('limit', '200')
      const q = params.toString()
      const { audits } = await api.listAudits(q ? '?' + q : '')
      setRows(audits || [])
    } catch (e) {
      setErr(e.message)
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    load()
  }, [])

  return (
    <div className="card">
      <h2>SWFP 操作记录 / 审计日志</h2>
      <p className="muted">展示 SWFP 路由的调用审计，creditCode 入参已脱敏。</p>
      <form className="toolbar" onSubmit={(e) => { e.preventDefault(); load() }}>
        <div>
          <label>检索（uuid / appKey）</label>
          <input value={keyword} onChange={(e) => setKeyword(e.target.value)} placeholder="全部" />
        </div>
        <div>
          <label>busiCode 筛选</label>
          <input value={busiCode} onChange={(e) => setBusiCode(e.target.value)} placeholder="如 10 / 1000 / 1007" />
        </div>
        <div>
          <button className="btn" type="submit" disabled={loading}>{loading ? '查询中…' : '查询'}</button>
        </div>
      </form>

      {err && <div className="error">{err}</div>}

      <div style={{ overflowX: 'auto' }}>
        <table>
          <thead>
            <tr>
              <th>时间</th><th>requestId</th><th>appKey</th><th>来源IP</th>
              <th>调用上游</th><th>查得数据</th><th>计费</th>
              <th>busiCode</th><th>上游code</th><th>上游uid</th><th>上游logId</th>
              <th>耗时(ms)</th><th>creditCode(脱敏)</th><th>tradeNo/reqid</th><th>错误</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((a) => (
              <tr key={a.id}>
                <td className="muted">{new Date(a.createdAt).toLocaleString()}</td>
                <td><code>{a.requestId}</code></td>
                <td>{a.appKey || '-'}</td>
                <td>{a.clientIp || '-'}</td>
                <td className={a.calledUpstream ? 'tag-ok' : 'tag-no'}>{a.calledUpstream ? '是' : '否'}</td>
                <td className={a.foundData ? 'tag-ok' : 'tag-no'}>{a.foundData ? '是' : '否'}</td>
                <td className={a.billed ? 'tag-ok' : 'tag-no'}>{a.billed ? '计' : '不计'}</td>
                <td>{a.busiCode}</td>
                <td>{a.upstreamCode || '-'}</td>
                <td>{a.upstreamUid || '-'}</td>
                <td className="muted">{a.upstreamLogId || '-'}</td>
                <td>{a.latencyMs}</td>
                <td className="muted">{a.idCardMask || '-'}</td>
                <td className="muted">{[a.tradeNo, a.reqid].filter(Boolean).join(' / ')}</td>
                <td className="tag-err">{a.errMsg || ''}</td>
              </tr>
            ))}
            {rows.length === 0 && (
              <tr><td colSpan="15" className="muted">暂无记录</td></tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  )
}
