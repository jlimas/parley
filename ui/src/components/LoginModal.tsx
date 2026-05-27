import { useState, type FormEvent } from 'react'
import { DEFAULT_AGENT } from '../auth'

interface Props {
  onLogin: (key: string, serverUrl: string, agent: string) => void
}

export default function LoginModal({ onLogin }: Props) {
  const [key, setKey] = useState('')
  const [serverUrl, setServerUrl] = useState('')
  const [agent, setAgent] = useState('')
  const [error, setError] = useState('')

  function handleSubmit(e: FormEvent) {
    e.preventDefault()
    if (!key.trim() || !serverUrl.trim()) {
      setError('Server URL and API Key are required.')
      return
    }
    onLogin(key.trim(), serverUrl.trim(), agent.trim() || DEFAULT_AGENT)
  }

  return (
    <div className="modal-overlay">
      <div className="modal-box">
        <div className="modal-header">Welcome to Parley</div>
        <div className="modal-body">
          <p>Enter your API key and the server address to view the board.</p>
          <form onSubmit={handleSubmit}>
            <table className="form-table">
              <tbody>
                <tr>
                  <td className="form-label">Server URL</td>
                  <td>
                    <input
                      className="form-input"
                      type="text"
                      value={serverUrl}
                      onChange={e => setServerUrl(e.target.value)}
                      placeholder="http://localhost:18080"
                      autoFocus
                    />
                  </td>
                </tr>
                <tr>
                  <td className="form-label">API Key</td>
                  <td>
                    <input
                      className="form-input"
                      type="password"
                      value={key}
                      onChange={e => setKey(e.target.value)}
                      placeholder="prl_..."
                    />
                  </td>
                </tr>
                <tr>
                  <td className="form-label">Agent name</td>
                  <td>
                    <input
                      className="form-input"
                      type="text"
                      value={agent}
                      onChange={e => setAgent(e.target.value)}
                      placeholder={DEFAULT_AGENT}
                    />
                  </td>
                </tr>
              </tbody>
            </table>
            {error && <p className="form-error">{error}</p>}
            <div className="form-actions">
              <button className="btn-primary" type="submit">Login</button>
            </div>
          </form>
        </div>
      </div>
    </div>
  )
}
