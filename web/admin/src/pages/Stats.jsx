import React, { useEffect, useState } from 'react'
import { api, timeRange } from '../api.js'

const COLS = 7

// 粒度选项：按年/月/日分桶。
const GRANS = [
  { id: 'day', label: '按日' },
  { id: 'month', label: '按月' },
  { id: 'year', label: '按年' },
]

// periodInputType 按粒度决定「限定周期」输入框类型。
function periodInputType(gran) {
  if (gran === 'year') return 'number'
  if (gran === 'month') return 'month'
  return 'date'
}

function rate(total, success) {
  if (!total) return '-'
  return ((success / total) * 100).toFixed(1) + '%'
}

// SourceBreakdown 展开某 (用户, 时间桶) 内的按源分布：这些请求是哪个源查成功的。
// 区间由该行的 bucket 标签换算（bucket 与周期输入框同格式，直接复用 timeRange）。
// 逐源明细仓储未装配时后端返回空列表，这里显示为提示而非报错。
function SourceBreakdown({ gran, bucket, appKey }) {
  const [rows, setRows] = useState(null)
  const [err, setErr] = useState('')

  useEffect(() => {
    let alive = true
    const { from, to } = timeRange(gran, bucket)
    const params = new URLSearchParams({ appKey })
    if (from) params.set('from', from)
    if (to) params.set('to', to)
    api
      .sourceStats('?' + params.toString())
      .then((d) => alive && setRows(d.sources || []))
      .catch((e) => alive && setErr(e.message))
    return () => {
      alive = false
    }
  }, [gran, bucket, appKey])

  if (err) return <div className="error">{err}</div>
  if (!rows) return <div className="muted">加载中…</div>
  if (rows.length === 0) return <div className="muted">无逐源明细（该区间未调用上游，或未启用明细落库）</div>

  return (
    <table>
      <thead>
        <tr>
          <th>源</th><th>段名</th><th>上游</th>
          <th>纳入寻源</th><th>查得</th><th>查无</th><th>失败</th><th>未调用</th>
        </tr>
      </thead>
      <tbody>
        {rows.map((s) => (
          <tr key={s.label}>
            <td>{s.source}</td>
            <td className="muted">{s.label}</td>
            <td className="muted">{s.provider || '-'}</td>
            <td>{s.calls}</td>
            <td className={s.ok ? 'tag-ok' : 'muted'}><strong>{s.ok}</strong></td>
            <td className="muted">{s.empty}</td>
            <td className={s.err ? 'tag-err' : 'muted'}>{s.err}</td>
            <td className="muted">{s.skipped}</td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

export default function Stats() {
  const [gran, setGran] = useState('day')
  const [period, setPeriod] = useState('') // 限定的具体年/月/日；空=全部时间
  const [keyword, setKeyword] = useState('')
  const [rows, setRows] = useState([])
  const [err, setErr] = useState('')
  const [loading, setLoading] = useState(false)
  const [openKey, setOpenKey] = useState(null)

  const load = async (g = gran, p = period, kw = keyword) => {
    setErr('')
    setLoading(true)
    try {
      const params = new URLSearchParams()
      params.set('granularity', g)
      if (kw) params.set('q', kw)
      const { from, to } = timeRange(g, p)
      if (from) params.set('from', from)
      if (to) params.set('to', to)
      const { stats } = await api.usageStats('?' + params.toString())
      setRows(stats || [])
      setOpenKey(null)
    } catch (e) {
      setErr(e.message)
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // 切粒度时清空已选周期（避免月输入残留给日粒度）并立即重查。
  const switchGran = (g) => {
    setGran(g)
    setPeriod('')
    load(g, '', keyword)
  }

  // 合计行：当前结果集的请求/成功总数。
  const totalReq = rows.reduce((s, r) => s + (r.total || 0), 0)
  const totalOk = rows.reduce((s, r) => s + (r.success || 0), 0)

  return (
    <div className="card">
      <h2>SWFP 用量统计（按时间）</h2>
      <p className="muted">
        按「年 / 月 / 日」查看每个用户的请求次数与成功次数。「请求次数」= 收到的全部请求（含鉴权/参数失败）；
        「成功次数」= 查得数据（与用户列表的「成功查得数」口径一致）。时间按北京时间（+08:00）分桶。
      </p>
      <p className="muted">
        点「按源展开」看该行这些请求是哪个源查成功的。按源那层来自逐源明细（一次请求 N 行），
        与主表的请求数不同量级，故分开聚合、不做合并。
        标「未鉴权」的行是在识别出用户之前就被拒的请求（签名错/未知 appKey/缺参），
        它们没有归属用户、也没调用上游，故不提供按源下钻。
      </p>

      <form className="toolbar" onSubmit={(e) => { e.preventDefault(); load() }}>
        <div>
          <label>粒度</label>
          <div className="version-switch" role="group" aria-label="统计粒度">
            {GRANS.map((g) => (
              <button
                key={g.id}
                type="button"
                className={'btn small' + (gran === g.id ? '' : ' ghost')}
                onClick={() => switchGran(g.id)}
              >
                {g.label}
              </button>
            ))}
          </div>
        </div>
        <div>
          <label>限定周期（可空=全部）</label>
          <input
            type={periodInputType(gran)}
            value={period}
            onChange={(e) => setPeriod(e.target.value)}
            placeholder={gran === 'year' ? '如 2026' : ''}
          />
        </div>
        <div>
          <label>用户检索（uuid / 名称 / 手机号）</label>
          <input value={keyword} onChange={(e) => setKeyword(e.target.value)} placeholder="全部用户" />
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
              <th>时间</th><th>uuid</th><th>名称</th>
              <th>请求次数</th><th>成功次数</th><th>成功率</th><th>按源</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((r, i) => {
              const k = r.bucket + '|' + r.appKey + '|' + i
              return (
                <React.Fragment key={k}>
                  <tr>
                    <td className="muted">{r.bucket}</td>
                    <td>{r.appKey ? <code>{r.appKey}</code> : <span className="muted">未鉴权</span>}</td>
                    <td>{r.name || '-'}</td>
                    <td><strong>{r.total}</strong></td>
                    <td><strong>{r.success}</strong></td>
                    <td className="muted">{rate(r.total, r.success)}</td>
                    <td>
                      {r.appKey ? (
                        <button
                          className="btn ghost small"
                          type="button"
                          onClick={() => setOpenKey(openKey === k ? null : k)}
                        >
                          {openKey === k ? '收起' : '按源展开'}
                        </button>
                      ) : (
                        <span className="muted">-</span>
                      )}
                    </td>
                  </tr>
                  {openKey === k && (
                    <tr>
                      <td colSpan={COLS}>
                        <SourceBreakdown gran={gran} bucket={r.bucket} appKey={r.appKey} />
                      </td>
                    </tr>
                  )}
                </React.Fragment>
              )
            })}
            {rows.length === 0 && (
              <tr><td colSpan={COLS} className="muted">暂无统计数据</td></tr>
            )}
          </tbody>
          {rows.length > 0 && (
            <tfoot>
              <tr>
                <td colSpan="3"><strong>合计（当前结果）</strong></td>
                <td><strong>{totalReq}</strong></td>
                <td><strong>{totalOk}</strong></td>
                <td className="muted">{rate(totalReq, totalOk)}</td>
                <td></td>
              </tr>
            </tfoot>
          )}
        </table>
      </div>
    </div>
  )
}
