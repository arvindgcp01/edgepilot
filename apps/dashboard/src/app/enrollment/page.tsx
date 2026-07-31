"use client";

import { useCallback, useEffect, useState } from "react";
import { AppShell } from "@/components/app-shell";
import { Button, Card, Empty, ErrorNote, Input, Spinner } from "@/components/ui";
import { can } from "@/lib/api/client";
import {
  createEnrollmentToken, listEnrollmentTokens, revokeEnrollmentToken,
  type EnrollmentToken,
} from "@/lib/api/devices";
import { absoluteTime, relativeTime } from "@/lib/format";

/** See the comment on DevicePage: the content mounts only once AppShell has a
 *  session, so nothing fetches before there is a token to fetch with. */
export default function EnrollmentPage() {
  return (
    <AppShell>
      <EnrollmentView />
    </AppShell>
  );
}

function EnrollmentView() {
  const [tokens, setTokens] = useState<EnrollmentToken[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<unknown>(null);
  /** The plaintext, held only until the operator navigates away. The server
   *  stores a SHA-256 and will never show it again. */
  const [minted, setMinted] = useState<EnrollmentToken | null>(null);

  const load = useCallback(async () => {
    try {
      setTokens((await listEnrollmentTokens()).data);
      setError(null);
    } catch (err) {
      setError(err);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { void load(); }, [load]);

  return (
    <>
      <h1 className="mb-6 text-xl font-semibold">Enrollment tokens</h1>

      {minted && <MintedToken token={minted} onDismiss={() => setMinted(null)} />}

      {can("enrollment_token:write") && (
        <CreateForm
          onCreated={async (token) => {
            setMinted(token);
            await load();
          }}
        />
      )}

      <ErrorNote error={error} />

      {loading ? (
        <Spinner />
      ) : tokens.length === 0 ? (
        <Empty>No enrollment tokens. Create one to install an agent.</Empty>
      ) : (
        <Card className="mt-4 overflow-x-auto p-0">
          <table className="w-full text-sm">
            <thead className="border-b border-surface-border text-left text-xs uppercase tracking-wide text-ink-faint">
              <tr>
                <th scope="col" className="px-4 py-2.5 font-medium">Token</th>
                <th scope="col" className="px-4 py-2.5 font-medium">Description</th>
                <th scope="col" className="px-4 py-2.5 font-medium">Uses</th>
                <th scope="col" className="px-4 py-2.5 font-medium">Auto-approve</th>
                <th scope="col" className="px-4 py-2.5 font-medium">Expires</th>
                <th scope="col" className="px-4 py-2.5" />
              </tr>
            </thead>
            <tbody>
              {tokens.map((token) => (
                <tr key={token.id} className="border-b border-surface-border/60 last:border-0">
                  <td className="px-4 py-2.5 font-mono text-xs">
                    {token.token_prefix}…
                    {!token.usable && <span className="ml-2 text-ink-faint">(unusable)</span>}
                  </td>
                  <td className="px-4 py-2.5 text-ink-muted">{token.description || "—"}</td>
                  <td className="px-4 py-2.5 tabular-nums text-ink-muted">
                    {token.use_count}
                    {token.max_uses ? ` / ${token.max_uses}` : ""}
                  </td>
                  <td className="px-4 py-2.5">
                    {token.auto_approve
                      ? <span className="text-degraded">yes</span>
                      : <span className="text-ink-muted">no</span>}
                  </td>
                  <td className="px-4 py-2.5 text-ink-muted" title={absoluteTime(token.expires_at)}>
                    {relativeTime(token.expires_at)}
                  </td>
                  <td className="px-4 py-2.5 text-right">
                    {token.usable && can("enrollment_token:write") && (
                      <Button
                        variant="ghost"
                        onClick={async () => {
                          if (!confirm("Revoke this token? Agents that have not enrolled yet will be refused.")) return;
                          await revokeEnrollmentToken(token.id);
                          await load();
                        }}
                      >
                        Revoke
                      </Button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </Card>
      )}
    </>
  );
}

function MintedToken({ token, onDismiss }: { token: EnrollmentToken; onDismiss: () => void }) {
  return (
    <Card className="mb-4 border-degraded/40 bg-degraded/5">
      <div className="mb-2 text-sm font-semibold text-degraded">
        Copy this token now — it is not shown again
      </div>
      <p className="mb-3 text-sm text-ink-muted">
        Only its SHA-256 is stored, so it cannot be recovered. If you lose it, revoke it and create another.
      </p>
      <code className="block break-all rounded bg-surface px-3 py-2 font-mono text-sm">
        {token.token}
      </code>
      <div className="mt-3 flex gap-2">
        <Button onClick={() => void navigator.clipboard?.writeText(token.token ?? "")}>
          Copy
        </Button>
        <Button variant="ghost" onClick={onDismiss}>Dismiss</Button>
      </div>
    </Card>
  );
}

function CreateForm({ onCreated }: { onCreated: (t: EnrollmentToken) => Promise<void> }) {
  const [description, setDescription] = useState("");
  const [hours, setHours] = useState(24);
  const [autoApprove, setAutoApprove] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);

  return (
    <Card>
      <h2 className="mb-3 text-sm font-semibold text-ink-muted">Create a token</h2>
      <form
        className="flex flex-wrap items-end gap-3"
        onSubmit={async (event) => {
          event.preventDefault();
          setBusy(true);
          setError(null);
          try {
            // Absolute expiry, computed here: the API takes a timestamp, and
            // the ceiling is enforced against the server's clock.
            const expiresAt = new Date(Date.now() + hours * 3600_000).toISOString();
            await onCreated(await createEnrollmentToken({
              description, expires_at: expiresAt, auto_approve: autoApprove,
            }));
            setDescription("");
          } catch (err) {
            setError(err);
          } finally {
            setBusy(false);
          }
        }}
      >
        <label className="block text-sm">
          <span className="mb-1 block text-ink-faint">Description</span>
          <Input
            required value={description} onChange={(e) => setDescription(e.target.value)}
            placeholder="London office rollout" className="w-64"
          />
        </label>

        <label className="block text-sm">
          <span className="mb-1 block text-ink-faint">Valid for (hours)</span>
          <Input
            type="number" min={1} max={720} value={hours} className="w-28"
            onChange={(e) => setHours(Number(e.target.value))}
          />
        </label>

        <label className="mb-2 flex items-center gap-2 text-sm text-ink-muted">
          <input
            type="checkbox" checked={autoApprove}
            onChange={(e) => setAutoApprove(e.target.checked)}
          />
          Approve devices automatically
        </label>

        <Button type="submit" disabled={busy} className="mb-1">
          {busy ? "Creating…" : "Create"}
        </Button>
      </form>

      {autoApprove && (
        <p className="mt-2 text-xs text-degraded">
          Any machine holding this token joins the fleet without review. Prefer leaving it off for production sites.
        </p>
      )}
      <ErrorNote error={error} />
    </Card>
  );
}
