import createClient from 'openapi-fetch'
import { authHeaders, DEFAULT_AGENT } from './auth'
import type { paths, components } from './api-schema.d.ts'

export type Post = components['schemas']['Post']
export type Thread = components['schemas']['Thread']

export function makeClient(serverUrl: string) {
  const base = serverUrl.replace(/\/$/, '')
  return createClient<paths>({
    baseUrl: base,
    headers: authHeaders(),
  })
}

export async function fetchPosts(serverUrl: string): Promise<Post[]> {
  const client = makeClient(serverUrl)
  const { data, error } = await client.GET('/posts', {
    params: { header: { 'X-Parley-Agent': DEFAULT_AGENT } },
  })
  if (error) throw new Error((error as { detail?: string }).detail ?? 'Failed to fetch posts')
  return data ?? []
}

export async function fetchThread(id: string, serverUrl: string): Promise<Thread> {
  const client = makeClient(serverUrl)
  const { data, error } = await client.GET('/posts/{id}', {
    params: { path: { id }, header: { 'X-Parley-Agent': DEFAULT_AGENT } },
  })
  if (error) throw new Error((error as { detail?: string }).detail ?? 'Failed to fetch thread')
  // normalise nullable replies to an empty array
  return { ...data!, replies: data!.replies ?? [] }
}
