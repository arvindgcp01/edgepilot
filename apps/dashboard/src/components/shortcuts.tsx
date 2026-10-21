"use client";

import { useRouter } from "next/navigation";
import { useEffect, useRef, useState } from "react";
import { Kbd, Modal } from "@/components/ui";

/** Two-key "go to" chords: press G, then a letter, within a second. */
const GO: Record<string, { href: string; label: string }> = {
  o: { href: "/", label: "Overview" },
  d: { href: "/devices", label: "Devices" },
  s: { href: "/sessions", label: "Sessions" },
  e: { href: "/enrollment", label: "Enrollment" },
  r: { href: "/reports", label: "Reports" },
  p: { href: "/policies", label: "Policies" },
  ",": { href: "/settings/general", label: "Settings" },
};

const SECTIONS: { title: string; rows: [React.ReactNode, string][] }[] = [
  {
    title: "Anywhere",
    rows: [
      [<><Kbd>⌘</Kbd><Kbd>K</Kbd></>, "Search devices, pages, and actions"],
      [<Kbd key="q">?</Kbd>, "Show this list"],
      ...Object.entries(GO).map(([key, g]): [React.ReactNode, string] => [<><Kbd>G</Kbd><Kbd>{key === "," ? "," : key.toUpperCase()}</Kbd></>, `Go to ${g.label}`]),
    ],
  },
  {
    title: "Device list",
    rows: [
      [<Kbd key="s">/</Kbd>, "Focus search"],
      [<><Kbd>J</Kbd><Kbd>K</Kbd></>, "Move down / up"],
      [<Kbd key="sp">Space</Kbd>, "Quick view"],
      [<Kbd key="en">↵</Kbd>, "Open device page"],
      [<Kbd key="x">X</Kbd>, "Select row"],
      [<Kbd key="esc">Esc</Kbd>, "Clear selection"],
    ],
  },
  {
    title: "Quick view & replay",
    rows: [
      [<><Kbd>↑</Kbd><Kbd>↓</Kbd></>, "Previous / next device"],
      [<Kbd key="sp2">Space</Kbd>, "Play / pause a recording"],
      [<><Kbd>←</Kbd><Kbd>→</Kbd></>, "Skip 5 seconds"],
    ],
  },
];

/**
 * The global keyboard layer: "?" for the cheat sheet, "G then X" to jump.
 * Ignored while typing in a field, so a hostname containing a "g" never
 * navigates anywhere.
 */
export function ShortcutsOverlay() {
  const router = useRouter();
  const [open, setOpen] = useState(false);
  const pendingG = useRef<number | null>(null);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const el = e.target as HTMLElement | null;
      if (["INPUT", "TEXTAREA", "SELECT"].includes(el?.tagName ?? "") || el?.isContentEditable) return;
      if (e.metaKey || e.ctrlKey || e.altKey) return;
      // xterm renders into a textarea, caught above; this also covers any
      // element that opts out explicitly.
      if (el?.closest("[data-shortcuts-off]")) return;

      if (e.key === "?") {
        e.preventDefault();
        setOpen((v) => !v);
        return;
      }
      if (pendingG.current && Date.now() - pendingG.current < 1000) {
        pendingG.current = null;
        const target = GO[e.key.toLowerCase()];
        if (target) {
          e.preventDefault();
          router.push(target.href);
        }
        return;
      }
      if (e.key === "g" || e.key === "G") pendingG.current = Date.now();
    };
    const onOpen = () => setOpen(true);
    document.addEventListener("keydown", onKey);
    window.addEventListener("edgepilot:shortcuts", onOpen);
    return () => {
      document.removeEventListener("keydown", onKey);
      window.removeEventListener("edgepilot:shortcuts", onOpen);
    };
  }, [router]);

  return (
    <Modal open={open} onClose={() => setOpen(false)} size="lg" title="Keyboard shortcuts" description="Work through the fleet without leaving the keyboard.">
      <div className="grid gap-8 sm:grid-cols-2">
        {SECTIONS.map((section) => (
          <section key={section.title} className={section.title === "Anywhere" ? "sm:row-span-2" : ""}>
            <h3 className="mb-3 text-2xs font-semibold uppercase tracking-wider text-ink-faint">{section.title}</h3>
            <ul className="space-y-2.5">
              {section.rows.map(([keys, label]) => (
                <li key={label} className="flex items-center justify-between gap-4 text-sm">
                  <span className="text-ink-muted">{label}</span>
                  <span className="flex shrink-0 gap-1">{keys}</span>
                </li>
              ))}
            </ul>
          </section>
        ))}
      </div>
    </Modal>
  );
}
