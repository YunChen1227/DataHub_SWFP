import React, { useState } from 'react'
import { getToken, setToken } from './api.js'
import Login from './pages/Login.jsx'
import Users from './pages/Users.jsx'
import Audits from './pages/Audits.jsx'

const TABS = [
  { id: 'users', label: '用户管理' },
  { id: 'audits', label: '操作记录' },
]

export default function App() {
  const [authed, setAuthed] = useState(!!getToken())
  const [tab, setTab] = useState('users')

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
        {tab === 'users' && <Users />}
        {tab === 'audits' && <Audits />}
      </div>
    </>
  )
}
