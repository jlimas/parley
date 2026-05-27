import { useQuery } from '@tanstack/react-query'
import { fetchPosts, type Post } from '../api'
import { formatDate, formatDay, isoDateKey } from '../utils'

interface Props {
  serverUrl: string
  onSelectThread: (id: string) => void
}

function threadLastActivity(post: Post, allReplies: Post[]): string {
  const times = [post.timestamp, ...allReplies.filter(r => r.parent_id === post.id).map(r => r.timestamp)]
  return times.reduce((max, t) => (t > max ? t : max))
}

export default function ForumIndex({ serverUrl, onSelectThread }: Props) {
  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ['posts', serverUrl],
    queryFn: () => fetchPosts(serverUrl),
    refetchInterval: 30_000,
  })

  const topLevel = (data ?? []).filter(p => !p.parent_id)
  const replies = (data ?? []).filter(p => !!p.parent_id)

  function replyCount(id: string) {
    return replies.filter(r => r.parent_id === id).length
  }

  // Sort newest-activity-first
  const sorted = [...topLevel].sort((a, b) => {
    const la = threadLastActivity(a, replies)
    const lb = threadLastActivity(b, replies)
    return lb > la ? 1 : lb < la ? -1 : 0
  })

  // Group by day of last activity
  const groups: { dayKey: string; dayIso: string; posts: Post[] }[] = []
  for (const post of sorted) {
    const lastIso = threadLastActivity(post, replies)
    const key = isoDateKey(lastIso)
    const last = groups[groups.length - 1]
    if (last && last.dayKey === key) {
      last.posts.push(post)
    } else {
      groups.push({ dayKey: key, dayIso: lastIso, posts: [post] })
    }
  }

  return (
    <div className="forum-wrap">
      <div className="category-header">
        <span>Board Index</span>
        <button className="link-btn small" onClick={() => refetch()}>Refresh</button>
      </div>

      {isLoading && <div className="status-row">Loading posts…</div>}
      {error && <div className="status-row error">Error: {(error as Error).message}</div>}
      {!isLoading && sorted.length === 0 && (
        <div className="status-row">No posts yet.</div>
      )}

      {sorted.length > 0 && (
        <table className="forum-table">
          <thead>
            <tr>
              <th className="col-topic">Topic</th>
              <th className="col-author">Author</th>
              <th className="col-replies">Replies</th>
              <th className="col-date">Last Activity</th>
            </tr>
          </thead>
          <tbody>
            {groups.map(group => (
              <>
                <tr key={`day-${group.dayKey}`} className="day-divider">
                  <td colSpan={4}>{formatDay(group.dayIso)}</td>
                </tr>
                {group.posts.map((post, i) => (
                  <PostRow
                    key={post.id}
                    post={post}
                    replyCount={replyCount(post.id)}
                    lastActivity={threadLastActivity(post, replies)}
                    odd={i % 2 === 1}
                    onClick={() => onSelectThread(post.id)}
                  />
                ))}
              </>
            ))}
          </tbody>
        </table>
      )}

      <div className="board-footer">
        Parley &mdash; Agent Message Board &mdash; {sorted.length} thread{sorted.length !== 1 ? 's' : ''}
      </div>
    </div>
  )
}

function PostRow({ post, replyCount, lastActivity, odd, onClick }: {
  post: Post
  replyCount: number
  lastActivity: string
  odd: boolean
  onClick: () => void
}) {
  return (
    <tr className={odd ? 'row-odd' : 'row-even'}>
      <td className="col-topic">
        <div className="topic-cell">
          <span className="topic-icon">&#9656;</span>
          <button className="topic-link" onClick={onClick}>
            {post.title ?? '(untitled)'}
          </button>
        </div>
      </td>
      <td className="col-author">{post.author}</td>
      <td className="col-replies">{replyCount}</td>
      <td className="col-date">{formatDate(lastActivity)}</td>
    </tr>
  )
}
