"use client";

/**
 * A file browser for one device, over SFTP.
 *
 * It runs on the same SSH access the terminal uses, so it can go anywhere the
 * logged-in account can — it opens in the account's home directory and lets the
 * operator walk up and down from there. Download fetches with the session token
 * and saves the blob; upload streams the chosen file straight to the current
 * directory.
 */
import { useCallback, useEffect, useRef, useState } from "react";
import { Button, Code, Drawer, EmptyState, ErrorNote, Icon, Spinner, useToast } from "@/components/ui";
import {
  downloadDeviceFile, listFiles, uploadDeviceFile,
  type FileEntry, type FileListing,
} from "@/lib/api/devices";
import { bytes as formatBytes, relativeTime } from "@/lib/format";
import { cn } from "@/lib/cn";

export function DeviceFiles({
  deviceId,
  hostname,
  open,
  onClose,
}: {
  deviceId: string;
  hostname: string;
  open: boolean;
  onClose: () => void;
}) {
  const { push } = useToast();
  const [listing, setListing] = useState<FileListing | null>(null);
  const [path, setPath] = useState<string | undefined>(undefined);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const fileInput = useRef<HTMLInputElement>(null);

  const load = useCallback(
    async (target?: string) => {
      setLoading(true);
      setError(null);
      try {
        const result = await listFiles(deviceId, target);
        setListing(result);
        setPath(result.path);
      } catch (err) {
        setError(err);
      } finally {
        setLoading(false);
      }
    },
    [deviceId],
  );

  useEffect(() => {
    if (open && !listing) void load();
    // Only on first open; navigation drives subsequent loads.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  const parent = path && path !== "/" ? path.replace(/\/[^/]+\/?$/, "") || "/" : null;

  async function onUpload(file: File) {
    if (!path) return;
    setBusy(`upload:${file.name}`);
    try {
      const result = await uploadDeviceFile(deviceId, path, file);
      push({ title: `Uploaded ${file.name}`, description: `${formatBytes(result.bytes)} → ${result.path}`, tone: "success" });
      await load(path);
    } catch (err) {
      push({ title: `Upload of ${file.name} failed`, description: err instanceof Error ? err.message : undefined, tone: "error" });
    } finally {
      setBusy(null);
    }
  }

  return (
    <Drawer
      open={open}
      onClose={onClose}
      title={`Files · ${hostname}`}
      width="lg"
      header={
        <div className="space-y-2 border-b border-surface-border px-5 py-3">
          <div className="flex items-center gap-2">
            <Button size="sm" variant="ghost" icon="arrow-up" disabled={!parent || loading} onClick={() => parent && void load(parent)}>
              Up
            </Button>
            <Button size="sm" variant="ghost" icon="refresh" disabled={loading} onClick={() => void load(path)}>
              <span className="sr-only">Refresh</span>
            </Button>
            <div className="ml-auto flex items-center gap-2">
              <input
                ref={fileInput}
                type="file"
                className="hidden"
                onChange={(e) => {
                  const file = e.target.files?.[0];
                  if (file) void onUpload(file);
                  e.target.value = "";
                }}
              />
              <Button size="sm" variant="primary" icon="arrow-up" loading={busy?.startsWith("upload:")} onClick={() => fileInput.current?.click()}>
                Upload here
              </Button>
            </div>
          </div>
          <div className="flex items-center gap-2 overflow-x-auto text-xs text-ink-muted">
            <Icon name="server" size={13} className="shrink-0 text-ink-faint" />
            <Code className="whitespace-nowrap">{path ?? "…"}</Code>
            {listing?.username && <span className="whitespace-nowrap text-ink-faint">as {listing.username}</span>}
          </div>
        </div>
      }
    >
      <ErrorNote error={error} onRetry={() => void load(path)} />
      {loading && !listing ? (
        <Spinner />
      ) : listing && listing.entries.length === 0 ? (
        <EmptyState icon="inbox" title="Empty directory" description="Nothing here. Use “Upload here” to add a file." />
      ) : (
        <ul className={cn("divide-y divide-surface-border", loading && "opacity-60")}>
          {sortEntries(listing?.entries ?? []).map((entry) => (
            <FileRow
              key={entry.name}
              entry={entry}
              busy={busy === `download:${entry.name}`}
              onOpenDir={() => path && void load(joinPath(path, entry.name))}
              onDownload={async () => {
                if (!path) return;
                setBusy(`download:${entry.name}`);
                try {
                  await downloadDeviceFile(deviceId, joinPath(path, entry.name), entry.name);
                } catch (err) {
                  push({ title: `Could not download ${entry.name}`, description: err instanceof Error ? err.message : undefined, tone: "error" });
                } finally {
                  setBusy(null);
                }
              }}
            />
          ))}
        </ul>
      )}
    </Drawer>
  );
}

function FileRow({
  entry,
  busy,
  onOpenDir,
  onDownload,
}: {
  entry: FileEntry;
  busy: boolean;
  onOpenDir: () => void;
  onDownload: () => void;
}) {
  return (
    <li className="flex items-center gap-3 px-5 py-2 hover:bg-surface-hover/40">
      <Icon name={entry.is_dir ? "layers" : "file-text"} size={16} className={entry.is_dir ? "text-accent-soft" : "text-ink-faint"} />
      {entry.is_dir ? (
        <button type="button" onClick={onOpenDir} className="min-w-0 flex-1 truncate text-left text-sm text-ink hover:text-accent-soft">
          {entry.name}
        </button>
      ) : (
        <span className="min-w-0 flex-1 truncate text-sm text-ink">{entry.name}</span>
      )}
      <span className="hidden shrink-0 text-xs tabular text-ink-faint sm:block">{entry.is_dir ? "—" : formatBytes(entry.size)}</span>
      <span className="hidden shrink-0 text-xs text-ink-faint md:block" title={entry.mod_time}>{relativeTime(entry.mod_time)}</span>
      <code className="hidden shrink-0 font-mono text-2xs text-ink-faint lg:block">{entry.mode}</code>
      {entry.is_dir ? (
        <Button size="sm" variant="ghost" icon="chevron-right" onClick={onOpenDir}>
          <span className="sr-only">Open</span>
        </Button>
      ) : (
        <Button size="sm" variant="ghost" icon="download" loading={busy} onClick={onDownload}>
          <span className="sr-only">Download {entry.name}</span>
        </Button>
      )}
    </li>
  );
}

function sortEntries(entries: FileEntry[]): FileEntry[] {
  return [...entries].sort((a, b) => {
    if (a.is_dir !== b.is_dir) return a.is_dir ? -1 : 1;
    return a.name.localeCompare(b.name, undefined, { numeric: true, sensitivity: "base" });
  });
}

function joinPath(dir: string, name: string): string {
  return dir.endsWith("/") ? `${dir}${name}` : `${dir}/${name}`;
}
