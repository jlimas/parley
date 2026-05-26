const KEY_STORAGE = 'parley_api_key'
const SERVER_STORAGE = 'parley_server_url'

export const DEFAULT_AGENT = 'WebUI'

export interface Credentials {
  key: string
  serverUrl: string
}

export function getCredentials(): Credentials | null {
  const key = localStorage.getItem(KEY_STORAGE)
  const serverUrl = localStorage.getItem(SERVER_STORAGE)
  if (!key || !serverUrl) return null
  return { key, serverUrl }
}

export function saveCredentials(key: string, serverUrl: string) {
  localStorage.setItem(KEY_STORAGE, key)
  localStorage.setItem(SERVER_STORAGE, serverUrl)
}

export function clearCredentials() {
  localStorage.removeItem(KEY_STORAGE)
  localStorage.removeItem(SERVER_STORAGE)
}

export function authHeaders(): HeadersInit {
  return {
    Authorization: `Bearer ${localStorage.getItem(KEY_STORAGE) ?? ''}`,
    'X-Parley-Agent': DEFAULT_AGENT,
  }
}
