// Auth state lives in localStorage so a lobby display survives reloads and power
// cuts without someone re-entering a password. The trade-off is that any XSS on
// this origin could read the tokens; keep the display build free of third-party scripts.
const STORAGE_KEY = 'qa.auth'

export function loadAuth() {
  try {
    return JSON.parse(localStorage.getItem(STORAGE_KEY)) || null
  } catch {
    return null
  }
}

function saveAuth(auth) {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(auth))
  } catch {
    // Storage may be unavailable (private mode); the session then lasts until reload.
  }
}

export function clearAuth() {
  try {
    localStorage.removeItem(STORAGE_KEY)
  } catch {}
}

function fromTokenResponse(r) {
  return {
    accessToken: r.access_token,
    refreshToken: r.refresh_token,
    expiresAt: Date.now() + r.expires_in * 1000,
    user: r.user,
  }
}

async function postJSON(path, body) {
  const res = await fetch(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
  const data = await res.json().catch(() => ({}))
  if (!res.ok) throw new Error(data.error || `HTTP ${res.status}`)
  return data
}

export async function login(email, password) {
  const auth = fromTokenResponse(await postJSON('/api/auth/login', { email, password }))
  saveAuth(auth)
  return auth
}

export async function logout() {
  const auth = loadAuth()
  clearAuth()
  if (auth?.refreshToken) {
    await postJSON('/api/auth/logout', { refresh_token: auth.refreshToken }).catch(() => {})
  }
}

// Refresh tokens are single-use: the server rotates them and treats a second use
// of the same token as theft (revoking every session). Concurrent callers must
// therefore share one in-flight refresh instead of each sending their own.
let inflightRefresh = null

export function refreshAuth() {
  if (!inflightRefresh) {
    inflightRefresh = (async () => {
      const current = loadAuth()
      if (!current?.refreshToken) throw new Error('not logged in')
      try {
        const auth = fromTokenResponse(await postJSON('/api/auth/refresh', { refresh_token: current.refreshToken }))
        saveAuth(auth)
        return auth
      } catch (err) {
        clearAuth()
        throw err
      }
    })().finally(() => {
      inflightRefresh = null
    })
  }
  return inflightRefresh
}

// getAccessToken returns a token valid for at least another 30 seconds.
export async function getAccessToken() {
  const auth = loadAuth()
  if (!auth) throw new Error('not logged in')
  if (auth.expiresAt - Date.now() > 30_000) return auth.accessToken
  return (await refreshAuth()).accessToken
}

export async function apiGet(path) {
  let res = await fetch(path, { headers: { Authorization: `Bearer ${await getAccessToken()}` } })
  if (res.status === 401) {
    const auth = await refreshAuth()
    res = await fetch(path, { headers: { Authorization: `Bearer ${auth.accessToken}` } })
  }
  const data = await res.json().catch(() => ({}))
  if (!res.ok) throw new Error(data.error || `HTTP ${res.status}`)
  return data
}
