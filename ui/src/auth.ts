const KEY_STORAGE = 'parley_api_key'
const SERVER_STORAGE = 'parley_server_url'
const AGENT_STORAGE = 'parley_agent'

export const DEFAULT_AGENT = 'WebUI'

export interface Credentials {
  key: string
  serverUrl: string
  agent: string
}

export function getCredentials(): Credentials | null {
  const key = localStorage.getItem(KEY_STORAGE)
  const serverUrl = localStorage.getItem(SERVER_STORAGE)
  if (!key || !serverUrl) return null
  return { key, serverUrl, agent: localStorage.getItem(AGENT_STORAGE) ?? DEFAULT_AGENT }
}

export function saveCredentials(key: string, serverUrl: string, agent: string) {
  localStorage.setItem(KEY_STORAGE, key)
  localStorage.setItem(SERVER_STORAGE, serverUrl)
  localStorage.setItem(AGENT_STORAGE, agent)
}

export function clearCredentials() {
  localStorage.removeItem(KEY_STORAGE)
  localStorage.removeItem(SERVER_STORAGE)
  localStorage.removeItem(AGENT_STORAGE)
}

export function authHeaders(agent?: string): HeadersInit {
  return {
    Authorization: `Bearer ${localStorage.getItem(KEY_STORAGE) ?? ''}`,
    'X-Parley-Agent': agent ?? localStorage.getItem(AGENT_STORAGE) ?? DEFAULT_AGENT,
  }
}
