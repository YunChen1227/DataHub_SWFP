import React, { useState } from 'react'
import { getToken, setToken } from './api.js'
import Login from './pages/Login.jsx'
import UpstreamAllowlist from './pages/UpstreamAllowlist.jsx'
import Users from './pages/Users.jsx'
import Audits from './pages/Audits.jsx'
import Stats from './pages/Stats.jsx'

const TABS = [
  { id: 'allowlist', label: '上游白名单' },
  { id: 'users', label: '用户管理' },
  { id: 'stats', label: '用量统计' },
  { id: 'audits', label: '操作记录' },
]

export default function App() {
  const [authed, setAuthed] = useState(!!getToken())
  const [tab, setTab] = useState('allowlist')

  if (!authed) {
    return <Login onLogin={() => setAuthed(true)} />
  }

  const logout = () => {
    setToken('')
    setAuthed(false)
  }

  return (
    <>
      <header className="app-header">
        <h1>DataHub_SWFP 管理后台</h1>
        <nav className="nav">
          {TABS.map((t) => (
            <button
              key={t.id}
              className={tab === t.id ? 'active' : ''}
              onClick={() => setTab(t.id)}
            >
              {t.label}
            </button>
          ))}
        </nav>
        <button className="btn ghost small" onClick={logout}>退出登录</button>
      </header>
      <div className="container">
        {tab === 'allowlist' && <UpstreamAllowlist />}
        {tab === 'users' && <Users />}
        {tab === 'stats' && <Stats />}
        {tab === 'audits' && <Audits />}
      </div>
    </>
  )
}
