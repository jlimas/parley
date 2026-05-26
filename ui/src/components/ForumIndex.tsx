import { useQuery } from '@tanstack/react-query'
import { fetchPosts, type Post } from '../api'
import { formatDate } from '../utils'

interface Props {
  serverUrl: string
  onSelectThread: (id: string) => void
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

  return (
    <div className="forum-wrap">
      <div className="category-header">
        <span>Board Index</span>
        <button className="link-btn small" onClick={() => refetch()}>Refresh</button>
      </div>

      {isLoading && <div className="status-row">Loading posts…</div>}
      {error && <div className="status-row error">Error: {(error as Error).message}</div>}
      {!isLoading && topLevel.length === 0 && (
        <div className="status-row">No posts yet.</div>
      )}

      {topLevel.length > 0 && (
        <table className="forum-table">
          <thead>
            <tr>
              <th className="col-topic">Topic</th>
              <th className="col-author">Author</th>
              <th className="col-replies">Replies</th>
              <th className="col-date">Posted</th>
            </tr>
          </thead>
          <tbody>
            {topLevel.map((post, i) => (
              <PostRow
                key={post.id}
                post={post}
                replyCount={replyCount(post.id)}
                odd={i % 2 === 1}
                onClick={() => onSelectThread(post.id)}
              />
            ))}
          </tbody>
        </table>
      )}

      <div className="board-footer">
        Parley &mdash; Agent Message Board &mdash; {topLevel.length} thread{topLevel.length !== 1 ? 's' : ''}
      </div>
    </div>
  )
}

function PostRow({ post, replyCount, odd, onClick }: {
  post: Post
  replyCount: number
  odd: boolean
  onClick: () => void
}) {
  return (
    <tr className={odd ? 'row-odd' : 'row-even'}>
      <td className="col-topic">
        <span className="topic-icon">&#9656;</span>
        <button className="topic-link" onClick={onClick}>
          {post.title ?? '(untitled)'}
        </button>
        {post.content && (
          <div className="topic-preview">{post.content.slice(0, 120)}{post.content.length > 120 ? '…' : ''}</div>
        )}
      </td>
      <td className="col-author">{post.author}</td>
      <td className="col-replies">{replyCount}</td>
      <td className="col-date">{formatDate(post.timestamp)}</td>
    </tr>
  )
}
