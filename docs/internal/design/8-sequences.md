# §8 Key sequences

Part of the [Wayfindr technical design](../design.md). Go code cites this file
as "design §8".

**`call_tool` with an expiring OAuth token**

```
Client ─call_tool(linear.create_issue)─► gateway ─GetTool/validate─► Manager.Call
  Manager ─session.CallTool─► SDK transport ─RoundTrip─► Broker.Transport
    expiry-60s < now → singleflight refresh → seal+PutCredential → set Bearer
  ─► upstream ─200─► CallToolResult unchanged ─► Client
  (401 → forced refresh + one replay; 403 insufficient_scope → needs_auth step-up)
```

**First-time OAuth**

```
wayfindr auth login slack ─POST /admin/auth/slack─► Flows.Start (lookup, OAuth check; flow enters the table as starting)
  probe 401 challenge → discover (PRM → AS metadata → legacy) → register (client_id | CIMD | DCR) → state+PKCE → arm
◄─ {flow_id, authorize_url}; CLI opens browser, long-polls /admin/auth/flows/{id}
Browser → AS consent → 302 http://127.0.0.1:7777/oauth/callback?code&state[&iss]
  Flows.HandleCallback: consume state, config still current?, check iss, exchange code (+verifier, resource),
    seal+store under the row lock (config checked again there)
  OnCredentialChanged → (daemon recovery, in the background) Manager.CredentialChanged → state ready
    (the new session's OnToolsChanged → Syncer.Request) → Syncer.Await
◄─ long-poll returns done with slack's row; CLI prints "Authorized slack." "slack is ready (42 tools)."
```

**A cloud agent signs in with OAuth** (ChatGPT, ADR 0022): the whole flow,
from the 401 challenge to a refresh, is the sequence diagram in
[§7.11](7.11-oauthserver.md).

**Revoke**

```
wayfindr auth logout slack ─DELETE /admin/credentials/slack─► recovery.revoke: take slack's turn
  (a recovery still running finishes first)
  Manager.Drop → session DELETE ─► Broker.Transport (token still stored) ─► upstream ends the session
  Broker.Revoke: cancel flows + delete the row (row lock) → OnCredentialChanged → recovery queued on the turn
  give up the turn → (recovery) Manager.CredentialChanged → no token → needs_auth (no_credential)
◄─ {connector, result: slack's row in needs_auth}; CLI prints "Revoked the credential of slack." and the row
```
