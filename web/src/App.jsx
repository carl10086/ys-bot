import { useEffect, useRef, useState } from 'react'

// 最小调试界面：单会话视图。
// 协议 = 讲座 15 hit-send：msg_id 客户端生成（幂等）；回显 = SSE 空信号 + after_seq 回源拉取。
export default function App() {
  const [sessionId, setSessionId] = useState(() => localStorage.getItem('ys-session-id') || '')
  const [messages, setMessages] = useState([])
  const [input, setInput] = useState('')
  const [status, setStatus] = useState('连接中…')
  const afterSeq = useRef(0)
  const bottomRef = useRef(null)

  // 进入即建/复用会话
  useEffect(() => {
    if (sessionId) return
    fetch('/api/sessions', { method: 'POST', body: '{}' })
      .then((r) => r.json())
      .then((d) => {
        localStorage.setItem('ys-session-id', d.id)
        setSessionId(d.id)
      })
      .catch((e) => setStatus('建会话失败: ' + e.message))
  }, [sessionId])

  // 回源拉取：把 seq > afterSeq 的消息按序接上
  const pull = async (sid) => {
    const r = await fetch(`/api/sessions/${sid}/messages?after_seq=${afterSeq.current}`)
    const d = await r.json()
    if (d.messages?.length) {
      afterSeq.current = d.messages[d.messages.length - 1].seq
      setMessages((prev) => [...prev, ...d.messages])
    }
  }

  // SSE 空信号 → 回源拉取（Watching）
  useEffect(() => {
    if (!sessionId) return
    pull(sessionId).then(() => setStatus('已连接'))
    const es = new EventSource(`/api/sessions/${sessionId}/events`)
    es.onmessage = () => pull(sessionId)
    es.onerror = () => setStatus('信号断开（自动重连）')
    return () => es.close()
  }, [sessionId])

  useEffect(() => {
    if (bottomRef.current) {
      bottomRef.current.scrollIntoView({ behavior: 'smooth' })
    }
  }, [messages])

  const send = async () => {
    const text = input.trim()
    if (!text || !sessionId) return
    setInput('')
    // msg_id 客户端生成：重发/双击/重试都安全
    await fetch(`/api/sessions/${sessionId}/messages`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ msg_id: crypto.randomUUID(), text }),
    })
    await pull(sessionId)
  }

  return (
    <div style={{ maxWidth: 720, margin: '24px auto', fontFamily: 'system-ui', padding: '0 16px' }}>
      <header style={{ display: 'flex', justifyContent: 'space-between', color: '#666', fontSize: 13 }}>
        <span>ys-sandbox 调试界面</span>
        <span>{status} · session {sessionId.slice(0, 8)}</span>
      </header>
      <main style={{ border: '1px solid #ddd', borderRadius: 8, padding: 16, minHeight: 400, margin: '12px 0' }}>
        {messages.map((m) => (
          <div key={m.seq} style={{ margin: '8px 0', textAlign: m.kind === 'user' ? 'right' : 'left' }}>
            <span
              style={{
                display: 'inline-block',
                padding: '8px 12px',
                borderRadius: 12,
                background: m.kind === 'user' ? '#2563eb' : '#f1f5f9',
                color: m.kind === 'user' ? '#fff' : '#111',
                maxWidth: '80%',
                textAlign: 'left',
                whiteSpace: 'pre-wrap',
              }}
            >
              {m.content?.text ?? JSON.stringify(m.content)}
            </span>
            <div style={{ fontSize: 11, color: '#999' }}>#{m.seq} {m.kind}</div>
          </div>
        ))}
        <div ref={bottomRef} />
      </main>
      <footer style={{ display: 'flex', gap: 8 }}>
        <input
          style={{ flex: 1, padding: '10px 12px', borderRadius: 8, border: '1px solid #ccc' }}
          value={input}
          onChange={(e) => setInput(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && send()}
          placeholder="发消息给 agent…（Enter 发送）"
        />
        <button onClick={send} style={{ padding: '10px 20px', borderRadius: 8 }}>发送</button>
      </footer>
    </div>
  )
}
