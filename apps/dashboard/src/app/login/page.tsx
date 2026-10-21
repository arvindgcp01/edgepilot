"use client";

import { useRouter } from "next/navigation";
import { useEffect, useState } from "react";
import { ApiError, signIn } from "@/lib/api/client";
import { useSession } from "@/components/session-provider";
import { Button, ErrorNote, Field, Icon, Input } from "@/components/ui";

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
    <main className="flex min-h-screen flex-col items-center justify-center px-4 py-12">
      <div className="w-full max-w-sm">
        <div className="mb-8 flex items-center gap-2.5">
          <span className="flex h-7 w-7 items-center justify-center rounded-md bg-accent text-white">
            <Icon name="grid" size={15} strokeWidth={2.25} />
          </span>
          <span className="text-base font-semibold tracking-tight">EdgePilot</span>
        </div>

        <h1 className="text-xl font-semibold tracking-tight">Sign in</h1>
        <p className="mb-6 mt-1 text-sm text-ink-muted">
          {mfaRequired ? "Enter the code from your authenticator app." : "Use your organization account to manage the fleet."}
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

          <Button type="submit" variant="primary" loading={submitting} className="w-full">
            {mfaRequired ? "Verify" : "Sign in"}
          </Button>
        </form>

        <p className="mt-8 text-center text-xs text-ink-faint">
          Access is logged. Sessions expire after inactivity.
        </p>
      </div>
    </main>
  );
}
