import { useEffect, useState } from 'react'
import { apiGet, loadAuth, login, logout } from './api.js'
import { selectCalled, selectWaitingByService } from './boardReducer.js'
import { useQueueSocket } from './useQueueSocket.js'

const BRANCH_KEY = 'qa.display.branch'

export default function App() {
  const [auth, setAuth] = useState(loadAuth)
  const [branch, setBranch] = useState(() => {
    try {
      return JSON.parse(localStorage.getItem(BRANCH_KEY))
    } catch {
      return null
    }
  })

  const chooseBranch = (b) => {
    setBranch(b)
    try {
      if (b) localStorage.setItem(BRANCH_KEY, JSON.stringify(b))
      else localStorage.removeItem(BRANCH_KEY)
    } catch {}
  }

  const signOut = async () => {
    await logout()
    chooseBranch(null)
    setAuth(null)
  }

  if (!auth) return <Login onLogin={setAuth} />
  if (!branch) return <BranchPicker onPick={chooseBranch} onSignOut={signOut} />
  return <Board branch={branch} onChangeBranch={() => chooseBranch(null)} onLoggedOut={() => setAuth(null)} />
}

function Login({ onLogin }) {
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState(null)
  const [busy, setBusy] = useState(false)

  const submit = async (e) => {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      onLogin(await login(email, password))
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <main className="center">
      <form className="card" onSubmit={submit}>
        <h1>Sıra Ekranı</h1>
        <label>
          E-posta
          <input type="email" value={email} onChange={(e) => setEmail(e.target.value)} autoComplete="username" required />
        </label>
        <label>
          Parola
          <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" required />
        </label>
        {error && <p className="error">{error}</p>}
        <button disabled={busy}>{busy ? 'Giriş yapılıyor…' : 'Giriş yap'}</button>
      </form>
    </main>
  )
}

function BranchPicker({ onPick, onSignOut }) {
  const [branches, setBranches] = useState(null)
  const [error, setError] = useState(null)
  const [filter, setFilter] = useState('')

  useEffect(() => {
    apiGet('/api/queue/branches')
      .then((d) => setBranches(d.branches))
      .catch((err) => setError(err.message))
  }, [])

  const shown = (branches || []).filter((b) =>
    `${b.code} ${b.name}`.toLocaleLowerCase('tr').includes(filter.toLocaleLowerCase('tr')),
  )

  return (
    <main className="center">
      <div className="card wide">
        <h1>Şube seçin</h1>
        {error && <p className="error">{error}</p>}
        {!branches && !error && <p className="muted">Yükleniyor…</p>}
        {branches && (
          <>
            <input placeholder="Ara…" value={filter} onChange={(e) => setFilter(e.target.value)} />
            <ul className="branch-list">
              {shown.map((b) => (
                <li key={b.id}>
                  <button onClick={() => onPick({ id: b.id, code: b.code, name: b.name })}>
                    <strong>{b.code}</strong> {b.name}
                  </button>
                </li>
              ))}
            </ul>
          </>
        )}
        <button className="link" onClick={onSignOut}>Çıkış yap</button>
      </div>
    </main>
  )
}

const STATUS_LABEL = {
  connecting: 'Bağlanıyor',
  recovering: 'Kaçırılan olaylar alınıyor',
  live: 'Canlı',
  reconnecting: 'Bağlantı koptu · yeniden deneniyor',
  error: 'Hata',
}

function Board({ branch, onChangeBranch, onLoggedOut }) {
  const { board, status, recovery, error } = useQueueSocket(branch.id, { onLoggedOut })
  const called = selectCalled(board)
  const waiting = selectWaitingByService(board)
  const notice = useTransientNotice(recovery)

  return (
    <main className="board">
      <header>
        <div>
          <h1>{branch.name}</h1>
          <span className="muted">{branch.code}</span>
        </div>
        <div className={`status status-${status}`} role="status">
          <span className="dot" />
          {STATUS_LABEL[status]}
          {board.lastEventId !== null && <span className="muted"> · #{board.lastEventId}</span>}
        </div>
        <button className="link" onClick={onChangeBranch}>Şube değiştir</button>
      </header>

      {error && <p className="error">{error}</p>}
      {notice && <div className="notice">{notice}</div>}

      <section className="called">
        <h2>Çağrılan</h2>
        {called.length === 0 && <p className="muted">Henüz çağrılan bilet yok.</p>}
        <ol>
          {called.map((t, i) => (
            <li key={t.id} className={i === 0 ? 'latest' : ''}>
              <span className="code">{t.display_code}</span>
              <span className="arrow">→</span>
              <span className="counter">{t.counter_name}</span>
            </li>
          ))}
        </ol>
      </section>

      <section className="waiting">
        <h2>Bekleyen</h2>
        {waiting.length === 0 && <p className="muted">Bekleyen bilet yok.</p>}
        <div className="services">
          {waiting.map(([code, list]) => (
            <div key={code} className="service">
              <div className="service-head">
                <strong>{code}</strong>
                <span>{list.length} kişi</span>
              </div>
              <div className="next">{list.slice(0, 5).map((t) => <span key={t.id}>{t.display_code}</span>)}</div>
            </div>
          ))}
        </div>
      </section>
    </main>
  )
}

// Shows a short message after each successful reconnect, then hides it.
function useTransientNotice(recovery) {
  const [notice, setNotice] = useState(null)
  useEffect(() => {
    if (!recovery) return
    setNotice(
      recovery.snapshot
        ? 'Bağlantı geri geldi · ekran güncel durumla yenilendi'
        : `Bağlantı geri geldi · ${recovery.replayed} kaçırılan olay uygulandı`,
    )
    const t = setTimeout(() => setNotice(null), 6000)
    return () => clearTimeout(t)
  }, [recovery])
  return notice
}
