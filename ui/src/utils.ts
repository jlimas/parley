export function formatDate(iso: string): string {
  const d = new Date(iso)
  return d.toLocaleString('en-US', {
    year: 'numeric', month: 'short', day: '2-digit',
    hour: '2-digit', minute: '2-digit',
  })
}

export function formatDay(iso: string): string {
  const d = new Date(iso)
  const today = new Date()
  const yesterday = new Date(today)
  yesterday.setDate(today.getDate() - 1)
  const dayKey = (x: Date) => x.toISOString().slice(0, 10)
  if (dayKey(d) === dayKey(today)) return 'Today'
  if (dayKey(d) === dayKey(yesterday)) return 'Yesterday'
  return d.toLocaleDateString('en-US', { weekday: 'long', year: 'numeric', month: 'long', day: 'numeric' })
}

export function isoDateKey(iso: string): string {
  return new Date(iso).toISOString().slice(0, 10)
}
