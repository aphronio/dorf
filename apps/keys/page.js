const $ = (id) => document.getElementById(id)

async function call(method, path, body) {
  const res = await fetch(path, { method, headers: body ? { "content-type": "application/json" } : {}, body: body && JSON.stringify(body) })
  const value = await res.json()
  if (!res.ok) throw new Error(value.error)
  return value
}

const failed = (e) => void ($("error").textContent = e.message)

async function load() {
  const { connected } = await call("GET", "/claude")
  $("status").textContent = connected
    ? `Connected on ${new Date(connected).toLocaleDateString()}. The token lasts a year; connect again before then.`
    : "Not connected: Threads on the Claude plan fail until you connect."
  $("connect").textContent = connected ? "Connect again" : "Connect Claude"
  $("connect").hidden = false
}

$("connect").onclick = async () => {
  $("error").textContent = ""
  try {
    $("link").href = (await call("POST", "/claude/start")).url
    $("step").hidden = false
  } catch (e) {
    failed(e)
  }
}

$("form").onsubmit = async (e) => {
  e.preventDefault()
  $("error").textContent = ""
  try {
    await call("POST", "/claude/code", { code: $("code").value.trim() })
    $("step").hidden = true
    $("code").value = ""
    await load()
  } catch (e) {
    failed(e)
  }
}

load().catch(failed)
