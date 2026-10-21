"use client";

import { useCallback, useEffect, useState } from "react";
import { AppShell } from "@/components/app-shell";
import {
  Badge, Button, Callout, Card, Checkbox, Code, ConfirmDialog, EmptyState, ErrorNote, Field, Input,
  Modal, PageHeader, Table, TableSkeleton, TBody, Td, Th, THead, Tr, useToast,
} from "@/components/ui";
import { can } from "@/lib/api/client";
import {
  createEnrollmentToken, listEnrollmentTokens, revokeEnrollmentToken,
  type EnrollmentToken,
} from "@/lib/api/devices";
import { absoluteTime, expiresIn, relativeTime } from "@/lib/format";

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
  const { push } = useToast();
  const [tokens, setTokens] = useState<EnrollmentToken[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<unknown>(null);
  const [creating, setCreating] = useState(false);
  const [revoking, setRevoking] = useState<EnrollmentToken | null>(null);
  const [busy, setBusy] = useState(false);
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

  const usable = tokens.filter((t) => t.usable);
  const spent = tokens.filter((t) => !t.usable);

  return (
    <>
      <PageHeader
        title="Enrollment"
        description="Tokens let a new Mac join the fleet. Each one is shown once, expires on a schedule you set, and can be revoked at any time."
        actions={
          can("enrollment_token:write") && (
            <Button variant="primary" icon="plus" onClick={() => setCreating(true)}>
              Create token
            </Button>
          )
        }
      />

      {minted && <MintedToken token={minted} onDismiss={() => setMinted(null)} />}

      <ErrorNote error={error} onRetry={() => void load()} />

      <Card padded={false} className="overflow-hidden">
        {loading ? (
          <TableSkeleton rows={4} columns={5} />
        ) : tokens.length === 0 ? (
          <EmptyState
            icon="key"
            title="No enrollment tokens"
            description="Create one, then run the one-line installer on the Mac. The device appears in Fleet as soon as the agent connects."
            action={can("enrollment_token:write") && <Button variant="primary" onClick={() => setCreating(true)}>Create token</Button>}
          />
        ) : (
          <Table>
            <THead>
              <tr>
                <Th>Token</Th>
                <Th>Description</Th>
                <Th align="right">Uses</Th>
                <Th>Approval</Th>
                <Th>Expires</Th>
                <Th>Created</Th>
                <Th align="right"><span className="sr-only">Actions</span></Th>
              </tr>
            </THead>
            <TBody>
              {[...usable, ...spent].map((token) => (
                <Tr key={token.id} className={token.usable ? "" : "text-ink-faint"}>
                  <Td>
                    <div className="flex items-center gap-2">
                      <Code>{token.token_prefix}…</Code>
                      {!token.usable && (
                        <Badge tone="neutral">{token.revoked_at ? "Revoked" : "Expired"}</Badge>
                      )}
                    </div>
                  </Td>
                  <Td className={token.usable ? "text-ink" : ""}>{token.description || "—"}</Td>
                  <Td align="right" className="tabular text-ink-muted">
                    {token.use_count}
                    {token.max_uses ? <span className="text-ink-faint"> / {token.max_uses}</span> : <span className="text-ink-faint"> / ∞</span>}
                  </Td>
                  <Td>
                    {token.auto_approve
                      ? <Badge tone="warning">Automatic</Badge>
                      : <span className="text-ink-muted">Manual review</span>}
                  </Td>
                  <Td className="text-ink-muted" title={absoluteTime(token.expires_at)}>
                    {expiresIn(token.expires_at)}
                  </Td>
                  <Td className="text-ink-muted" title={absoluteTime(token.created_at)}>{relativeTime(token.created_at)}</Td>
                  <Td align="right">
                    {token.usable && can("enrollment_token:write") && (
                      <Button size="sm" variant="danger" onClick={() => setRevoking(token)}>Revoke</Button>
                    )}
                  </Td>
                </Tr>
              ))}
            </TBody>
          </Table>
        )}
      </Card>

      <CreateTokenModal
        open={creating}
        onClose={() => setCreating(false)}
        onCreated={async (token) => {
          setCreating(false);
          setMinted(token);
          await load();
        }}
      />

      <ConfirmDialog
        open={revoking !== null}
        onClose={() => setRevoking(null)}
        title="Revoke this token?"
        confirmLabel="Revoke token"
        loading={busy}
        description={
          <>
            Any Mac that has not finished enrolling with <Code>{revoking?.token_prefix}…</Code> will be refused.
            Devices that already enrolled are not affected.
          </>
        }
        onConfirm={async () => {
          if (!revoking) return;
          setBusy(true);
          try {
            await revokeEnrollmentToken(revoking.id);
            push({ title: "Token revoked", tone: "success" });
            setRevoking(null);
            await load();
          } catch (err) {
            push({ title: "Could not revoke the token", description: err instanceof Error ? err.message : undefined, tone: "error" });
          } finally {
            setBusy(false);
          }
        }}
      />
    </>
  );
}

function MintedToken({ token, onDismiss }: { token: EnrollmentToken; onDismiss: () => void }) {
  const { push } = useToast();
  const command = `curl -fsSL https://edgepilot.visionify.ai/install.sh | sudo bash -s -- ${token.token ?? ""}`;
  return (
    <Callout tone="warning" title="Copy this token now — it is not shown again" className="mb-4">
      <p className="mt-0.5">Only its SHA-256 is stored, so it cannot be recovered. If you lose it, revoke it and create another.</p>
      <div className="mt-3 space-y-2">
        <div className="text-xs text-ink-faint">Run on the Mac to install and enroll in one step:</div>
        <code className="block break-all rounded-md border border-surface-border bg-surface px-3 py-2 font-mono text-xs text-ink">
          {command}
        </code>
      </div>
      <div className="mt-3 flex gap-2">
        <Button
          variant="primary"
          size="sm"
          icon="copy"
          onClick={() => {
            void navigator.clipboard?.writeText(command);
            push({ title: "Install command copied", tone: "success" });
          }}
        >
          Copy command
        </Button>
        <Button
          size="sm"
          onClick={() => {
            void navigator.clipboard?.writeText(token.token ?? "");
            push({ title: "Token copied", tone: "success" });
          }}
        >
          Copy token only
        </Button>
        <Button size="sm" variant="ghost" onClick={onDismiss}>Dismiss</Button>
      </div>
    </Callout>
  );
}

function CreateTokenModal({
  open,
  onClose,
  onCreated,
}: {
  open: boolean;
  onClose: () => void;
  onCreated: (t: EnrollmentToken) => Promise<void>;
}) {
  const [description, setDescription] = useState("");
  const [hours, setHours] = useState(24);
  const [maxUses, setMaxUses] = useState<string>("");
  const [autoApprove, setAutoApprove] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);

  useEffect(() => {
    if (!open) {
      setDescription("");
      setHours(24);
      setMaxUses("");
      setAutoApprove(false);
      setError(null);
    }
  }, [open]);

  async function submit(event: React.FormEvent) {
    event.preventDefault();
    setBusy(true);
    setError(null);
    try {
      // Absolute expiry, computed here: the API takes a timestamp, and the
      // ceiling is enforced against the server's clock.
      const expiresAt = new Date(Date.now() + hours * 3600_000).toISOString();
      await onCreated(
        await createEnrollmentToken({
          description,
          expires_at: expiresAt,
          auto_approve: autoApprove,
          max_uses: maxUses ? Number(maxUses) : null,
        }),
      );
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  }

  return (
    <Modal
      open={open}
      onClose={onClose}
      title="Create enrollment token"
      description="A token is a credential until the moment it is redeemed. Keep its lifetime as short as the rollout allows."
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={busy}>Cancel</Button>
          <Button variant="primary" type="submit" form="create-token" loading={busy}>Create token</Button>
        </>
      }
    >
      <form id="create-token" onSubmit={submit} className="space-y-4">
        <Field label="Description" required hint="Where or why this token is being used — it appears in the audit log.">
          <Input required value={description} onChange={(e) => setDescription(e.target.value)} placeholder="Chicago DC rollout" />
        </Field>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Valid for" hint="Hours. Maximum 720 (30 days).">
            <Input type="number" min={1} max={720} value={hours} onChange={(e) => setHours(Number(e.target.value))} />
          </Field>
          <Field label="Maximum enrollments" hint="Leave empty for unlimited within the lifetime.">
            <Input type="number" min={1} value={maxUses} onChange={(e) => setMaxUses(e.target.value)} placeholder="Unlimited" />
          </Field>
        </div>
        <Checkbox
          checked={autoApprove}
          onChange={(e) => setAutoApprove(e.target.checked)}
          label="Approve devices automatically"
          description="Any machine holding this token joins the fleet without review. Leave off for production sites."
        />
        {autoApprove && (
          <Callout tone="warning">
            Auto-approval skips the review step entirely. Pair it with a single-use limit and a short lifetime.
          </Callout>
        )}
        <ErrorNote error={error} />
      </form>
    </Modal>
  );
}
