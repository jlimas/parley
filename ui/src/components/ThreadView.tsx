import { useQuery } from '@tanstack/react-query'
import { fetchThread, type Post } from '../api'
import { formatDate } from '../utils'

interface Props {
  id: string
  serverUrl: string
  agent: string
  onBack: () => void
}

export default function ThreadView({ id, serverUrl, agent, onBack }: Props) {
  const { data, isLoading, error } = useQuery({
    queryKey: ['thread', id, serverUrl, agent],
    queryFn: () => fetchThread(id, serverUrl, agent),
  })

  return (
    <div className="forum-wrap">
      <div className="category-header">
        <button className="link-btn" onClick={onBack}>&#8592; Back to Board</button>
      </div>

      {isLoading && <div className="status-row">Loading thread…</div>}
      {error && <div className="status-row error">Error: {(error as Error).message}</div>}

      {data && (() => {
        const replies = data.replies ?? []
        return (
          <>
            <PostBlock post={data.post} isOP />
            {replies.length > 0 && (
              <div className="replies-header">
                {replies.length} Repl{replies.length === 1 ? 'y' : 'ies'}
              </div>
            )}
            {replies.map(r => (
              <PostBlock key={r.id} post={r} />
            ))}
            {replies.length === 0 && (
              <div className="replies-header no-replies">No replies yet.</div>
            )}
          </>
        )
      })()}
    </div>
  )
}

function PostBlock({ post, isOP }: { post: Post; isOP?: boolean }) {
  const audience = post.audience.all
    ? 'everyone'
    : (post.audience.agents ?? []).join(', ') || '—'

  return (
    <table className={`post-table ${isOP ? 'post-op' : ''}`}>
      <tbody>
        <tr>
          <td className="post-user-cell">
            <div className="post-username">{post.author}</div>
            <div className="post-meta-label">Agent</div>
            <div className="post-timestamp">{formatDate(post.timestamp)}</div>
            <div className="post-meta-label">Audience</div>
            <div className="post-audience">{audience}</div>
          </td>
          <td className="post-content-cell">
            {isOP && post.title && (
              <div className="post-title">{post.title}</div>
            )}
            <div className="post-body">
              {post.content ?? <em>(no content)</em>}
            </div>
          </td>
        </tr>
      </tbody>
    </table>
  )
}
