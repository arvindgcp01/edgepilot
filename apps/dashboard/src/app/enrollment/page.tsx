"use client";

import Link from "next/link";
import { useSearchParams } from "next/navigation";
import { Suspense, useCallback, useEffect, useState } from "react";
import { AppShell } from "@/components/app-shell";
import {
  Badge, Button, Callout, Card, Checkbox, Code, ConfirmDialog, EmptyState, ErrorNote, Field, Icon, Input,
  Modal, PageHeader, Segmented, Table, TableSkeleton, Tabs, TBody, Td, Th, THead, Tr, useToast, type IconName,
} from "@/components/ui";
import { cn } from "@/lib/cn";
import { can } from "@/lib/api/client";
import {
  createEnrollmentToken, listDevices, listEnrollmentTokens, revokeEnrollmentToken,
  type Device, type EnrollmentToken,
} from "@/lib/api/devices";
import { absoluteTime, expiresIn, relativeTime } from "@/lib/format";

/** See the comment on DevicePage: the content mounts only once AppShell has a
 *  session, so nothing fetches before there is a token to fetch with. */
export default function EnrollmentPage() {
  return (
    <AppShell>
      {/* useSearchParams needs a Suspense boundary for static rendering. */}
      <Suspense>
        <EnrollmentView />
      </Suspense>
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
  const [minted, setMinted] = useState<{ token: EnrollmentToken; at: number } | null>(null);
  const [tab, setTab] = useState<"active" | "inactive">("active");
  const params = useSearchParams();

  // Arriving from "Enroll device" anywhere in the app opens the form directly.
  useEffect(() => {
    if (params.get("new") === "1" && can("enrollment_token:write")) setCreating(true);
  }, [params]);

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

  const shown = tab === "active" ? usable : spent;

  return (
    <>
      <PageHeader
        title="Enrollment"
        description="Add devices to your fleet with short-lived tokens. Each token is shown once, expires on a schedule you set, and can be revoked at any time."
        actions={
          can("enrollment_token:write") && (
            <Button variant="primary" icon="plus" onClick={() => setCreating(true)}>
              New token
            </Button>
          )
        }
      />

      {minted ? (
        <InstallPanel token={minted.token} since={minted.at} onDismiss={() => setMinted(null)} />
      ) : (
        <HowItWorks onStart={can("enrollment_token:write") ? () => setCreating(true) : undefined} />
      )}

      <ErrorNote error={error} onRetry={() => void load()} />

      <Card padded={false} className="overflow-hidden">
        <div className="px-5 pt-3">
          <Tabs
            aria-label="Token status"
            value={tab}
            onChange={setTab}
            className="border-b-0"
            tabs={[
              { value: "active", label: "Active", count: usable.length },
              { value: "inactive", label: "Expired & revoked", count: spent.length },
            ]}
          />
        </div>
        <div className="border-t border-surface-border" />
        {loading ? (
          <TableSkeleton rows={4} columns={5} />
        ) : shown.length === 0 ? (
          <EmptyState
            icon="key"
            title={tab === "active" ? "No active tokens" : "Nothing here yet"}
            description={
              tab === "active"
                ? "Create a token, then run the one-line installer on the device. It appears in Devices as soon as the agent connects."
                : "Tokens that expire or are revoked are kept here for the audit trail."
            }
            action={tab === "active" && can("enrollment_token:write") && <Button variant="primary" icon="plus" onClick={() => setCreating(true)}>New token</Button>}
          />
        ) : (
          <Table>
            <THead>
              <tr>
                <Th>Token</Th>
                <Th>Description</Th>
                <Th>Usage</Th>
                <Th>Approval</Th>
                <Th>{tab === "active" ? "Expires" : "Status"}</Th>
                <Th>Created</Th>
                <Th align="right"><span className="sr-only">Actions</span></Th>
              </tr>
            </THead>
            <TBody>
              {shown.map((token) => (
                <Tr key={token.id}>
                  <Td>
                    <div className="flex items-center gap-2.5">
                      <span className={cn("flex h-8 w-8 items-center justify-center rounded-lg", token.usable ? "bg-accent-subtle text-accent" : "bg-surface-hover text-ink-faint")}>
                        <Icon name="key" size={14} />
                      </span>
                      <Code>{token.token_prefix}…</Code>
                    </div>
                  </Td>
                  <Td className={token.usable ? "font-medium text-ink" : "text-ink-muted"}>{token.description || "—"}</Td>
                  <Td>
                    <UsageMeter used={token.use_count} max={token.max_uses} />
                  </Td>
                  <Td>
                    {token.auto_approve
                      ? <Badge tone="warning">Automatic</Badge>
                      : <Badge tone="neutral">Manual review</Badge>}
                  </Td>
                  <Td className="whitespace-nowrap text-ink-muted" title={absoluteTime(token.expires_at)}>
                    {token.usable ? expiresIn(token.expires_at) : <Badge tone="neutral">{token.revoked_at ? "Revoked" : "Expired"}</Badge>}
                  </Td>
                  <Td className="whitespace-nowrap text-ink-muted" title={absoluteTime(token.created_at)}>{relativeTime(token.created_at)}</Td>
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
          setMinted({ token, at: Date.now() });
          setTab("active");
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

function UsageMeter({ used, max }: { used: number; max?: number | null }) {
  if (!max) {
    return (
      <span className="text-sm tabular text-ink-muted">
        {used} <span className="text-ink-faint">/ unlimited</span>
      </span>
    );
  }
  const pct = Math.min(100, Math.round((used / max) * 100));
  return (
    <div className="flex w-32 items-center gap-2">
      <div className="h-1.5 flex-1 overflow-hidden rounded-full bg-surface-hover">
        <div className={cn("h-full rounded-full", pct >= 100 ? "bg-offline" : "bg-accent")} style={{ width: `${pct}%` }} />
      </div>
      <span className="text-xs tabular text-ink-muted">{used}/{max}</span>
    </div>
  );
}

const STEPS: { title: string; body: string; icon: IconName }[] = [
  { title: "Create a token", body: "Short-lived and use-limited. It is the only thing a device needs to join.", icon: "key" },
  { title: "Run the installer", body: "One command on the device. No inbound ports, no VPN — it dials out.", icon: "terminal" },
  { title: "Approve", body: "The device appears within seconds. Review it, approve it, and it's managed.", icon: "check-circle" },
];

function HowItWorks({ onStart }: { onStart?: () => void }) {
  return (
    <Card padded={false} className="mb-6 overflow-hidden">
      <ol className="grid md:grid-cols-3">
        {STEPS.map((step, i) => (
          <li
            key={step.title}
            className={cn("relative flex gap-4 p-5", i > 0 && "border-t border-surface-border md:border-l md:border-t-0")}
          >
            <span className="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl bg-accent-subtle text-accent">
              <Icon name={step.icon} size={18} />
            </span>
            <div>
              <div className="text-2xs font-semibold uppercase tracking-wider text-ink-faint">Step {i + 1}</div>
              <div className="mt-0.5 text-sm font-semibold text-ink">{step.title}</div>
              <p className="mt-1 text-sm text-ink-muted">{step.body}</p>
              {i === 0 && onStart && (
                <button type="button" onClick={onStart} className="mt-2 inline-flex items-center gap-1 text-sm font-medium text-accent hover:text-accent-hover">
                  Create one now <Icon name="arrow-right" size={14} />
                </button>
              )}
            </div>
          </li>
        ))}
      </ol>
    </Card>
  );
}

/**
 * Step two and three, shown right after a token is minted: the install command,
 * and a live watch for the device it admits.
 *
 * The watch polls the device list for anything enrolled after the token was
 * created — by time, since the API does not record which token admitted a
 * device. It is a convenience — the real record is the device list — so it
 * stops after ten minutes rather than polling forever in a forgotten tab.
 */
function InstallPanel({ token, since, onDismiss }: { token: EnrollmentToken; since: number; onDismiss: () => void }) {
  const { push } = useToast();
  const command = `curl -fsSL https://edgepilot.visionify.ai/install.sh | sudo bash -s -- ${token.token ?? ""}`;
  const [joined, setJoined] = useState<Device[]>([]);
  const [watching, setWatching] = useState(true);

  useEffect(() => {
    if (!can("device:read")) return;
    let cancelled = false;
    const started = Date.now();
    const tick = async () => {
      try {
        const page = await listDevices({ sort: "-created_at", limit: 10 });
        const fresh = page.data.filter((d) => new Date(d.created_at).getTime() >= since - 5_000);
        if (!cancelled) setJoined(fresh);
      } catch {
        // A failed poll is not worth surfacing; the next one will try again.
      }
      if (Date.now() - started > 10 * 60_000 && !cancelled) setWatching(false);
    };
    void tick();
    const timer = setInterval(() => void tick(), 4_000);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [since]);

  const copy = (value: string, what: string) => {
    void navigator.clipboard?.writeText(value);
    push({ title: `${what} copied`, tone: "success" });
  };

  return (
    <Card padded={false} className="mb-6 overflow-hidden border-accent/30 ring-4 ring-accent/5">
      <div className="flex flex-wrap items-start justify-between gap-4 border-b border-surface-border bg-gradient-to-r from-indigo-500/10 via-violet-500/5 to-transparent px-6 py-5">
        <div className="flex items-start gap-4">
          <span className="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl bg-accent text-white shadow-sm shadow-accent/30">
            <Icon name="check" size={18} strokeWidth={2.5} />
          </span>
          <div>
            <h2 className="text-base font-semibold text-ink">Token created — install the agent</h2>
            <p className="mt-0.5 text-sm text-ink-muted">
              {token.description ? <span className="font-medium text-ink">{token.description}</span> : "This token"} is ready.
              Copy it now: it is stored only as a hash and <strong className="text-ink">will not be shown again</strong>.
            </p>
          </div>
        </div>
        <Button size="sm" variant="ghost" icon="x" onClick={onDismiss}>Done</Button>
      </div>

      <div className="grid gap-6 p-6 lg:grid-cols-5">
        <div className="lg:col-span-3">
          <div className="mb-2 flex items-center justify-between">
            <span className="text-sm font-medium text-ink">Run this on the device</span>
            <span className="text-xs text-ink-faint">macOS · requires admin</span>
          </div>
          <div className="group relative rounded-xl bg-slate-900 p-4 pr-14 dark:bg-black/40">
            <code className="block break-all font-mono text-[13px] leading-relaxed text-slate-100">
              <span className="select-none text-emerald-400">$ </span>
              {command}
            </code>
            <button
              type="button"
              onClick={() => copy(command, "Install command")}
              className="absolute right-3 top-3 rounded-lg bg-white/10 p-2 text-slate-200 transition-colors hover:bg-white/20"
              aria-label="Copy install command"
            >
              <Icon name="copy" size={15} />
            </button>
          </div>
          <div className="mt-3 flex flex-wrap gap-2">
            <Button size="sm" variant="primary" icon="copy" onClick={() => copy(command, "Install command")}>Copy command</Button>
            <Button size="sm" icon="key" onClick={() => copy(token.token ?? "", "Token")}>Copy token only</Button>
          </div>
        </div>

        <div className="lg:col-span-2">
          {/* Matched by time, not by token: the API does not say which token
              enrolled a device, so this lists everything that joined since. */}
          <div className="mb-2 text-sm font-medium text-ink">New devices since this token was created</div>
          <div className="rounded-xl border border-dashed border-surface-border-strong bg-surface p-4">
            {joined.length === 0 ? (
              <div className="flex items-center gap-3 py-2">
                {watching ? (
                  <span className="relative flex h-3 w-3">
                    <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-accent opacity-50" />
                    <span className="relative inline-flex h-3 w-3 rounded-full bg-accent" />
                  </span>
                ) : (
                  <Icon name="clock" className="text-ink-faint" />
                )}
                <div>
                  <div className="text-sm font-medium text-ink">{watching ? "Waiting for the device…" : "Stopped watching"}</div>
                  <div className="text-xs text-ink-faint">
                    {watching ? "It will appear here a few seconds after the installer finishes." : "Check Devices — it may have joined since."}
                  </div>
                </div>
              </div>
            ) : (
              <ul className="space-y-2">
                {joined.map((d) => (
                  <li key={d.id} className="flex animate-slide-up items-center gap-3 rounded-lg bg-surface-raised p-2.5 shadow-card">
                    <span className="flex h-8 w-8 items-center justify-center rounded-lg bg-online/10 text-online">
                      <Icon name="check" size={15} strokeWidth={2.25} />
                    </span>
                    <div className="min-w-0 flex-1">
                      <div className="truncate text-sm font-semibold text-ink">{d.hostname || d.serial_number}</div>
                      <div className="text-xs text-ink-faint">{d.approved ? "Joined and approved" : "Joined — awaiting approval"}</div>
                    </div>
                    <Link href={`/devices/${d.id}`} className="text-sm font-medium text-accent hover:text-accent-hover">
                      {d.approved ? "View" : "Review"}
                    </Link>
                  </li>
                ))}
              </ul>
            )}
          </div>
        </div>
      </div>
    </Card>
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
      title="New enrollment token"
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
        <div className="space-y-4">
          <Field label="Valid for" hint="Keep it as short as the rollout allows.">
            <Segmented<string>
              aria-label="Token lifetime"
              value={String(hours)}
              onChange={(v) => setHours(Number(v))}
              options={[
                { value: "1", label: "1 hour" },
                { value: "24", label: "1 day" },
                { value: "168", label: "1 week" },
                { value: "720", label: "30 days" },
              ]}
            />
          </Field>
          <Field label="Maximum enrollments" hint="Leave empty for unlimited within the lifetime.">
            <Input className="sm:w-48" type="number" min={1} value={maxUses} onChange={(e) => setMaxUses(e.target.value)} placeholder="Unlimited" />
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
