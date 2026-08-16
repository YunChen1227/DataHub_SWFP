import React, { useEffect, useState } from 'react'
import { api } from '../api.js'

const COLS = 18

// fen 把「分」显示为元；0 显示为 -（未产生金额）。
function fen(v) {
  if (!v) return '-'
  return (v / 100).toFixed(2)
}

const FEE_LABEL = {
  both: '发票+税务',
  invoice: '单发票',
  tax: '单税务',
  none: '不计费',
}

const STATUS_LABEL = {
  ok: '查得',
  empty: '查无',
  error: '失败',
  skipped: '未调用',
}

// SourceCalls 展开某次请求的逐源调用明细：上游成本对账与「为什么按这档收费」的一手证据。
function SourceCalls({ requestId }) {
  const [calls, setCalls] = useState(null)
  const [err, setErr] = useState('')

  useEffect(() => {
    let alive = true
    api
      .listUpstreamCalls(requestId)
      .then((d) => alive && setCalls(d.calls || []))
      .catch((e) => alive && setErr(e.message))
    return () => {
      alive = false
    }
  }, [requestId])

  if (err) return <div className="error">{err}</div>
  if (!calls) return <div className="muted">加载中…</div>
  if (calls.length === 0) return <div className="muted">无逐源明细（该请求未调用上游，或未启用明细落库）</div>

  return (
    <table>
      <thead>
        <tr>
          <th>序</th><th>源</th><th>段名</th><th>对外编号</th><th>上游</th>
          <th>维度</th><th>结果</th><th>上游code</th><th>上游uid</th>
          <th>耗时(ms)</th><th>成本(元)</th><th>说明</th>
        </tr>
      </thead>
      <tbody>
        {calls.map((c) => (
          <tr key={c.label + '-' + c.seq}>
            <td>{c.seq || '-'}</td>
            <td>{c.source}</td>
            <td className="muted">{c.label}</td>
            <td>{c.alias}</td>
            <td className="muted">{c.provider}</td>
            <td className="muted">
              {[c.dims?.invoice && '发票', c.dims?.tax && '税务'].filter(Boolean).join('+') || '-'}
            </td>
            <td className={c.status === 'ok' ? 'tag-ok' : c.status === 'error' ? 'tag-err' : 'tag-no'}>
              {STATUS_LABEL[c.status] || c.status}
            </td>
            <td>{c.code || '-'}</td>
            <td className="muted">{c.uid || '-'}</td>
            <td>{c.latencyMs || 0}</td>
            <td>{fen(c.costFen)}</td>
            <td className="muted">{c.reason || c.msg || ''}</td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

export default function Audits() {
  const [rows, setRows] = useState([])
  const [err, setErr] = useState('')
  const [keyword, setKeyword] = useState('')
  const [busiCode, setBusiCode] = useState('')
  const [loading, setLoading] = useState(false)
  const [openId, setOpenId] = useState(null)

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
      setOpenId(null)
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
      <p className="muted">
        展示 SWFP 路由的调用审计，creditCode 入参已脱敏。收费标准按【实际查得的维度】判定：
        请求两项而只查得发票时按【单发票】收费。点「逐源明细」查看本次调了哪些源、各源成本与跳过原因。
      </p>
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
              <th>请求维度</th><th>实得维度</th><th>收费标准</th><th>应收(元)</th><th>上游成本(元)</th>
              <th>调用上游</th><th>查得数据</th>
              <th>busiCode</th><th>上游code</th><th>上游uid</th>
              <th>耗时(ms)</th><th>creditCode(脱敏)</th><th>错误</th><th>明细</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((a) => (
              <React.Fragment key={a.id}>
                <tr>
                  <td className="muted">{new Date(a.createdAt).toLocaleString()}</td>
                  <td><code>{a.requestId}</code></td>
                  <td>{a.appKey || '-'}</td>
                  <td>{a.clientIp || '-'}</td>
                  <td className="muted">{a.reqScope || '-'}</td>
                  <td className="muted">{a.dataScope || '-'}</td>
                  <td>{FEE_LABEL[a.feeStandard] || a.feeStandard || '-'}</td>
                  <td>{fen(a.amountFen)}</td>
                  <td>{fen(a.upstreamCostFen)}</td>
                  <td className={a.calledUpstream ? 'tag-ok' : 'tag-no'}>{a.calledUpstream ? '是' : '否'}</td>
                  <td className={a.foundData ? 'tag-ok' : 'tag-no'}>{a.foundData ? '是' : '否'}</td>
                  <td>{a.busiCode}</td>
                  <td>{a.upstreamCode || '-'}</td>
                  <td>{a.upstreamUid || '-'}</td>
                  <td>{a.latencyMs}</td>
                  <td className="muted">{a.idCardMask || '-'}</td>
                  <td className="tag-err">{a.errMsg || ''}</td>
                  <td>
                    <button className="btn" type="button" onClick={() => setOpenId(openId === a.id ? null : a.id)}>
                      {openId === a.id ? '收起' : '逐源明细'}
                    </button>
                  </td>
                </tr>
                {openId === a.id && (
                  <tr>
                    <td colSpan={COLS}>
                      <SourceCalls requestId={a.requestId} />
                    </td>
                  </tr>
                )}
              </React.Fragment>
            ))}
            {rows.length === 0 && (
              <tr><td colSpan={COLS} className="muted">暂无记录</td></tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  )
}
