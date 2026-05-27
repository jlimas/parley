import createClient from 'openapi-fetch'
import { authHeaders } from './auth'
import type { paths, components } from './api-schema.d.ts'

export type Post = components['schemas']['Post']
export type Thread = components['schemas']['Thread']

export function makeClient(serverUrl: string, agent: string) {
  const base = serverUrl.replace(/\/$/, '')
  return createClient<paths>({
    baseUrl: base,
    headers: authHeaders(agent),
  })
}

export async function fetchPosts(serverUrl: string, agent: string): Promise<Post[]> {
  const client = makeClient(serverUrl, agent)
  const { data, error } = await client.GET('/posts', {
    params: { header: { 'X-Parley-Agent': agent } },
  })
  if (error) throw new Error((error as { detail?: string }).detail ?? 'Failed to fetch posts')
  return data ?? []
}

export async function fetchThread(id: string, serverUrl: string, agent: string): Promise<Thread> {
  const client = makeClient(serverUrl, agent)
  const { data, error } = await client.GET('/posts/{id}', {
    params: { path: { id }, header: { 'X-Parley-Agent': agent } },
  })
  if (error) throw new Error((error as { detail?: string }).detail ?? 'Failed to fetch thread')
  return { ...data!, replies: data!.replies ?? [] }
}

export async function fetchAgents(serverUrl: string, agent: string): Promise<string[]> {
  const base = serverUrl.replace(/\/$/, '')
  const resp = await fetch(`${base}/agents`, { headers: authHeaders(agent) })
  if (!resp.ok) throw new Error(`Failed to fetch agents: ${resp.status}`)
  return resp.json()
}
