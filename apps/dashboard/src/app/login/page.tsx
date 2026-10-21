"use client";

import { useRouter } from "next/navigation";
import { useEffect, useState } from "react";
import { ApiError, signIn } from "@/lib/api/client";
import { useSession } from "@/components/session-provider";
import { Button, ErrorNote, Field, Icon, Input, type IconName } from "@/components/ui";

export default function LoginPage() {
  const router = useRouter();
  const { principal, loading } = useSession();

  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [mfaCode, setMfaCode] = useState("");
  const [mfaRequired, setMfaRequired] = useState(false);
  const [organization, setOrganization] = useState("");
  const [orgRequired, setOrgRequired] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [submitting, setSubmitting] = useState(false);

  useEffect(() => {
    if (!loading && principal) router.replace("/");
  }, [loading, principal, router]);

  async function submit(event: React.FormEvent) {
    event.preventDefault();
    setSubmitting(true);
    setError(null);
    try {
      await signIn(email, password, {
        ...(mfaCode ? { mfa_code: mfaCode } : {}),
        ...(organization ? { organization } : {}),
      });
      router.replace("/");
    } catch (err) {
      // A missing second factor is not a failure to report as one: the password
      // was correct, and the form needs to grow a field rather than shout.
      if (err instanceof ApiError && err.status === 428) {
        setMfaRequired(true);
        setError(null);
      } else {
        setError(err);
        // The server never says which tenant a sign-in resolved to, so after a
        // failure the operator is offered the field that might fix it.
        if (err instanceof ApiError && err.status === 401) setOrgRequired(true);
      }
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <main className="grid min-h-screen lg:grid-cols-2">
      <div className="flex flex-col px-6 py-10 sm:px-12">
        <div className="flex items-center gap-2.5">
          <span className="flex h-9 w-9 items-center justify-center rounded-xl bg-gradient-to-br from-indigo-500 to-violet-600 text-white shadow-sm shadow-indigo-500/30">
            <Icon name="layers" size={18} strokeWidth={2} />
          </span>
          <span className="text-lg font-semibold tracking-tight">EdgePilot</span>
        </div>

        <div className="mx-auto flex w-full max-w-sm flex-1 flex-col justify-center py-12">
          <h1 className="text-3xl font-semibold tracking-tight">{mfaRequired ? "Two-step verification" : "Welcome back"}</h1>
          <p className="mb-8 mt-2 text-sm text-ink-muted">
            {mfaRequired ? "Enter the 6-digit code from your authenticator app." : "Sign in to manage your fleet."}
          </p>

          <form onSubmit={submit} className="space-y-4">
            {!mfaRequired && (
              <>
                <Field label="Email" htmlFor="email">
                  <Input
                    id="email" type="email" name="email" autoComplete="username" required autoFocus
                    value={email} onChange={(e) => setEmail(e.target.value)}
                  />
                </Field>
                <Field label="Password" htmlFor="password">
                  <Input
                    id="password" type="password" name="password" autoComplete="current-password" required
                    value={password} onChange={(e) => setPassword(e.target.value)}
                  />
                </Field>
              </>
            )}

            {mfaRequired && (
              <Field label="Authentication code" htmlFor="mfa">
                <Input
                  id="mfa" inputMode="numeric" pattern="[0-9]{6}" maxLength={6} autoFocus required
                  autoComplete="one-time-code" className="font-mono text-base tracking-[0.3em]"
                  value={mfaCode} onChange={(e) => setMfaCode(e.target.value)}
                />
              </Field>
            )}

            {orgRequired && !mfaRequired && (
              <Field
                label="Organization"
                htmlFor="org"
                hint="Only needed if this server hosts several organizations on one address."
              >
                <Input id="org" placeholder="acme" value={organization} onChange={(e) => setOrganization(e.target.value)} />
              </Field>
            )}

            <ErrorNote error={error} />

            <Button type="submit" variant="primary" loading={submitting} className="h-10 w-full">
              {mfaRequired ? "Verify" : "Sign in"}
            </Button>
          </form>

          <p className="mt-8 flex items-center justify-center gap-1.5 text-xs text-ink-faint">
            <Icon name="lock" size={12} /> Access is logged. Sessions expire after inactivity.
          </p>
        </div>

        <p className="text-xs text-ink-faint">© Visionify, Inc.</p>
      </div>

      <BrandPanel />
    </main>
  );
}

/**
 * The right half of the sign-in page: what EdgePilot is, for the person who has
 * just been sent a link to it. Decorative — hidden below the large breakpoint,
 * where the form needs the whole width.
 */
function BrandPanel() {
  const features: { icon: IconName; title: string; body: string }[] = [
    { icon: "globe", title: "No inbound ports", body: "Devices dial out over a single mTLS link. Nothing to open on the customer's firewall." },
    { icon: "activity", title: "Live health", body: "CPU, memory, disk, and presence for every machine, updated in real time." },
    { icon: "terminal", title: "Real SSH, fully audited", body: "End-to-end encrypted sessions with who, when, and from where on record." },
  ];
  return (
    <div className="relative hidden overflow-hidden bg-gradient-to-br from-indigo-600 via-indigo-700 to-violet-800 lg:flex lg:flex-col lg:justify-center lg:px-16">
      <div className="pointer-events-none absolute -right-24 -top-24 h-96 w-96 rounded-full bg-white/10 blur-3xl" aria-hidden />
      <div className="pointer-events-none absolute -bottom-32 -left-16 h-96 w-96 rounded-full bg-violet-400/20 blur-3xl" aria-hidden />
      <div
        className="pointer-events-none absolute inset-0 opacity-[0.07]"
        style={{ backgroundImage: "radial-gradient(circle at 1px 1px, white 1px, transparent 0)", backgroundSize: "24px 24px" }}
        aria-hidden
      />

      <div className="relative max-w-md">
        <span className="inline-flex items-center gap-1.5 rounded-full bg-white/10 px-3 py-1 text-xs font-medium text-indigo-100 ring-1 ring-white/20">
          <Icon name="sparkles" size={13} /> Fleet management for the edge
        </span>
        <h2 className="mt-6 text-4xl font-semibold leading-tight tracking-tight text-white">
          Every device at every site, in one place.
        </h2>
        <ul className="mt-10 space-y-6">
          {features.map((f) => (
            <li key={f.title} className="flex gap-4">
              <span className="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl bg-white/10 text-white ring-1 ring-white/20">
                <Icon name={f.icon} size={18} />
              </span>
              <div>
                <div className="font-semibold text-white">{f.title}</div>
                <p className="mt-0.5 text-sm text-indigo-100/80">{f.body}</p>
              </div>
            </li>
          ))}
        </ul>
      </div>
    </div>
  );
}
