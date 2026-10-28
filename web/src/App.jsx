import { useEffect, useRef, useState } from 'react'

// ChatGPT 式布局的调试界面：左侧倒序会话列表（选择/rename/删除/新建），右侧消息流。
// 协议 = 讲座 15 hit-send：msg_id 客户端生成（幂等）；回显 = SSE 空信号 + after_seq 回源拉取。
// userId 固定 local（单机调试）；标题由首条消息自动命名，可手动 rename。

const USER_ID = 'local'

export default function App() {
  const [sessions, setSessions] = useState([])
  const [sessionId, setSessionId] = useState(() => localStorage.getItem('ys-session-id') || '')
  const [sessionInfo, setSessionInfo] = useState(null) // {user_id, title}
  const [messages, setMessages] = useState([])
  const [input, setInput] = useState('')
  const [status, setStatus] = useState('连接中…')
  const [thinking, setThinking] = useState(false)
  const [editingId, setEditingId] = useState(null) // 侧栏内联 rename 目标
  const [editText, setEditText] = useState('')
  const afterSeq = useRef(0)
  const bottomRef = useRef(null)

  const loadSessions = async () => {
    const r = await fetch(`/api/sessions?user_id=${USER_ID}`)
    if (!r.ok) throw new Error('拉会话列表失败: HTTP ' + r.status)
    const d = await r.json()
    setSessions(d.sessions || [])
    return d.sessions || []
  }

  const selectSession = (id) => {
    localStorage.setItem('ys-session-id', id)
    afterSeq.current = 0
    setMessages([])
    setThinking(false)
    setSessionInfo(null)
    setStatus('连接中…')
    setSessionId(id)
  }

  const newSession = async () => {
    const r = await fetch('/api/sessions', { method: 'POST' })
    if (!r.ok) throw new Error('建会话失败: HTTP ' + r.status)
    const d = await r.json()
    await loadSessions()
    selectSession(d.id)
  }

  const renameSession = async (id, title) => {
    setEditingId(null)
    const old = sessions.find((s) => s.id === id)
    if (!title.trim() || title.trim() === old?.title) return
    await fetch(`/api/sessions/${id}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ title: title.trim() }),
    })
    await loadSessions()
    if (id === sessionId) setSessionInfo((prev) => (prev ? { ...prev, title: title.trim() } : prev))
  }

  const deleteSession = async (id) => {
    if (!window.confirm('删除这个会话及其全部消息？')) return
    await fetch(`/api/sessions/${id}`, { method: 'DELETE' })
    const rest = await loadSessions()
    if (id === sessionId) {
      localStorage.removeItem('ys-session-id')
      if (rest.length > 0) {
        selectSession(rest[0].id)
      } else {
        setSessionId('')
        setSessionInfo(null)
        setMessages([])
        afterSeq.current = 0
        newSession().catch((e) => setStatus('建会话失败: ' + e.message))
      }
    }
  }

  // 进入：拉列表；恢复 localStorage 里的会话；没有或失效（清库后）则自动建新的
  useEffect(() => {
    loadSessions()
      .then(() => {
        if (!sessionId) return newSession()
      })
      .catch((e) => setStatus(e.message))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // 切换会话：拉会话信息；失效则从本地状态摘掉
  useEffect(() => {
    if (!sessionId) return
    fetch(`/api/sessions/${sessionId}`)
      .then((r) => {
        if (!r.ok) throw new Error('gone')
        return r.json()
      })
      .then((d) => setSessionInfo({ user_id: d.user_id, title: d.title }))
      .catch(() => {
        localStorage.removeItem('ys-session-id')
        setSessionId('')
        loadSessions().then((rest) => {
          if (rest.length > 0) selectSession(rest[0].id)
          else return newSession()
        })
      })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sessionId])

  // 回源拉取：把 seq > afterSeq 的消息按序接上
  const pull = async (sid) => {
    const r = await fetch(`/api/sessions/${sid}/messages?after_seq=${afterSeq.current}`)
    const d = await r.json()
    if (d.messages?.length) {
      afterSeq.current = d.messages[d.messages.length - 1].seq
      setMessages((prev) => [...prev, ...d.messages])
      if (d.messages[d.messages.length - 1].kind === 'assistant') setThinking(false)
    }
  }

  // SSE 空信号 → 回源拉取（Watching）
  useEffect(() => {
    if (!sessionId || !sessionInfo) return
    pull(sessionId).then(() => setStatus('已连接'))
    const es = new EventSource(`/api/sessions/${sessionId}/events`)
    es.onmessage = () => pull(sessionId)
    es.onerror = () => setStatus('信号断开（自动重连）')
    return () => es.close()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sessionId, sessionInfo])

  useEffect(() => {
    if (bottomRef.current) {
      bottomRef.current.scrollIntoView({ behavior: 'smooth' })
    }
  }, [messages])

  const send = async () => {
    const text = input.trim()
    if (!text || !sessionId) return
    setInput('')
    setThinking(true)
    // msg_id 客户端生成：重发/双击/重试都安全
    await fetch(`/api/sessions/${sessionId}/messages`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ msg_id: crypto.randomUUID(), text }),
    })
    await pull(sessionId)
    await loadSessions() // 首条消息可能触发自动命名；活跃排序也要刷新
    // 自动命名发生在服务端，同步一下当前标题
    const r = await fetch(`/api/sessions/${sessionId}`)
    if (r.ok) {
      const d = await r.json()
      setSessionInfo({ user_id: d.user_id, title: d.title })
    }
  }

  const label = sessionInfo ? `${sessionInfo.title || '新会话'} · ${sessionInfo.user_id} · ${sessionId.slice(0, 8)}` : '…'

  return (
    <div style={{ display: 'flex', height: '100vh', fontFamily: 'system-ui' }}>
      {/* 侧栏：会话列表（最近活跃倒序） */}
      <aside
        style={{
          width: 260,
          flexShrink: 0,
          background: '#f8fafc',
          borderRight: '1px solid #e2e8f0',
          display: 'flex',
          flexDirection: 'column',
        }}
      >
        <div style={{ padding: 12 }}>
          <button
            onClick={() => newSession().catch((e) => setStatus(e.message))}
            style={{
              width: '100%',
              padding: '10px 12px',
              borderRadius: 8,
              border: '1px solid #cbd5e1',
              background: '#fff',
              cursor: 'pointer',
              textAlign: 'left',
            }}
          >
            ＋ 新会话
          </button>
        </div>
        <div style={{ flex: 1, overflowY: 'auto', padding: '0 8px 12px' }}>
          {sessions.map((s) => (
            <SessionItem
              key={s.id}
              session={s}
              active={s.id === sessionId}
              editing={editingId === s.id}
              editText={editText}
              onSelect={() => s.id !== sessionId && selectSession(s.id)}
              onStartEdit={() => {
                setEditingId(s.id)
                setEditText(s.title)
              }}
              onEditChange={setEditText}
              onCommitEdit={(title) => renameSession(s.id, title)}
              onCancelEdit={() => setEditingId(null)}
              onDelete={() => deleteSession(s.id)}
            />
          ))}
        </div>
      </aside>

      {/* 主区：消息流 + 输入框 */}
      <div style={{ flex: 1, display: 'flex', flexDirection: 'column', minWidth: 0 }}>
        <header
          style={{
            display: 'flex',
            justifyContent: 'space-between',
            color: '#666',
            fontSize: 13,
            padding: '10px 16px',
            borderBottom: '1px solid #e2e8f0',
          }}
        >
          <span>ys-sandbox 调试界面</span>
          <span>
            {status} · {label}
          </span>
        </header>
        <main style={{ flex: 1, overflowY: 'auto', padding: '16px 0' }}>
          <div style={{ maxWidth: 760, margin: '0 auto', padding: '0 16px' }}>
            {messages.map((m) => (
              <div key={m.seq} style={{ margin: '8px 0', textAlign: m.kind === 'user' ? 'right' : 'left' }}>
                <span
                  title={`#${m.seq} ${m.kind}${m.turn_id ? ' · ' + m.turn_id : ''}`}
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
                <div style={{ fontSize: 11, color: '#999' }}>
                  #{m.seq} {m.kind}
                </div>
              </div>
            ))}
            {thinking && <div style={{ color: '#999', fontSize: 13 }}>agent 思考中…</div>}
            <div ref={bottomRef} />
          </div>
        </main>
        <footer style={{ borderTop: '1px solid #e2e8f0', padding: '12px 0' }}>
          <div style={{ maxWidth: 760, margin: '0 auto', padding: '0 16px', display: 'flex', gap: 8 }}>
            <input
              style={{ flex: 1, padding: '10px 12px', borderRadius: 8, border: '1px solid #ccc' }}
              value={input}
              onChange={(e) => setInput(e.target.value)}
              onKeyDown={(e) => e.key === 'Enter' && send()}
              placeholder="发消息给 agent…（Enter 发送）"
            />
            <button onClick={send} style={{ padding: '10px 20px', borderRadius: 8 }}>
              发送
            </button>
          </div>
        </footer>
      </div>
    </div>
  )
}

// 侧栏单个会话项：选中高亮；双击标题进入内联 rename；hover 出删除按钮。
function SessionItem({
  session,
  active,
  editing,
  editText,
  onSelect,
  onStartEdit,
  onEditChange,
  onCommitEdit,
  onCancelEdit,
  onDelete,
}) {
  const [hover, setHover] = useState(false)
  return (
    <div
      onClick={onSelect}
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
      style={{
        display: 'flex',
        alignItems: 'center',
        gap: 4,
        padding: '8px 10px',
        borderRadius: 8,
        marginBottom: 2,
        cursor: 'pointer',
        fontSize: 14,
        background: active ? '#e2e8f0' : hover ? '#eef2f7' : 'transparent',
      }}
    >
      {editing ? (
        <input
          autoFocus
          value={editText}
          onChange={(e) => onEditChange(e.target.value)}
          onClick={(e) => e.stopPropagation()}
          onKeyDown={(e) => {
            if (e.key === 'Enter') onCommitEdit(editText)
            if (e.key === 'Escape') onCancelEdit()
          }}
          onBlur={() => onCommitEdit(editText)}
          style={{ flex: 1, minWidth: 0, padding: '2px 6px', fontSize: 14 }}
        />
      ) : (
        <span
          onDoubleClick={(e) => {
            e.stopPropagation()
            onStartEdit()
          }}
          title="双击重命名"
          style={{
            flex: 1,
            minWidth: 0,
            overflow: 'hidden',
            textOverflow: 'ellipsis',
            whiteSpace: 'nowrap',
            color: session.title ? '#111' : '#94a3b8',
          }}
        >
          {session.title || '新会话'}
        </span>
      )}
      {hover && !editing && (
        <>
          <button
            onClick={(e) => {
              e.stopPropagation()
              onStartEdit()
            }}
            title="重命名"
            style={{ border: 'none', background: 'none', cursor: 'pointer', color: '#94a3b8', padding: '0 2px' }}
          >
            ✎
          </button>
          <button
            onClick={(e) => {
              e.stopPropagation()
              onDelete()
            }}
            title="删除会话"
            style={{ border: 'none', background: 'none', cursor: 'pointer', color: '#94a3b8', padding: '0 2px' }}
          >
            ✕
          </button>
        </>
      )}
    </div>
  )
}
