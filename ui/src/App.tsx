import { useState } from 'react'
import { getCredentials, saveCredentials, clearCredentials, DEFAULT_AGENT } from './auth'
import LoginModal from './components/LoginModal'
import ForumIndex from './components/ForumIndex'
import ThreadView from './components/ThreadView'

type View = { page: 'index' } | { page: 'thread'; id: string }

export default function App() {
  const [creds, setCreds] = useState(getCredentials)
  const [view, setView] = useState<View>({ page: 'index' })

  function handleLogin(key: string, serverUrl: string) {
    saveCredentials(key, serverUrl)
    setCreds({ key, serverUrl })
  }

  function handleLogout() {
    clearCredentials()
    setCreds(null)
    setView({ page: 'index' })
  }

  if (!creds) return <LoginModal onLogin={handleLogin} />

  return (
    <div className="page-wrap">
      <div className="forum-header">
        <span className="forum-title">Parley</span>
        <span className="forum-agent">
          <span className="forum-server">{creds.serverUrl}</span>
          {' · '}
          agent <strong>{DEFAULT_AGENT}</strong>
          {' · '}
          <button className="link-btn" onClick={handleLogout}>sign out</button>
        </span>
      </div>

      {view.page === 'index' && (
        <ForumIndex
          serverUrl={creds.serverUrl}
          onSelectThread={(id) => setView({ page: 'thread', id })}
        />
      )}
      {view.page === 'thread' && (
        <ThreadView
          id={view.id}
          serverUrl={creds.serverUrl}
          onBack={() => setView({ page: 'index' })}
        />
      )}
    </div>
  )
}
