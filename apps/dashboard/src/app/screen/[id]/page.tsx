"use client";

/**
 * The pop-out remote screen.
 *
 * A standalone, chrome-free window that fills itself with one device's desktop.
 * Opened from the device page's "open in a new window" control, it re-uses the
 * root layout's session — the new window bootstraps from the same-origin
 * HttpOnly refresh cookie, so nothing has to be carried across from the opener.
 *
 * There is no AppShell here on purpose: this window is the screen, nothing else.
 */
import { useParams, useRouter } from "next/navigation";
import { useEffect, useState } from "react";
import { useSession } from "@/components/session-provider";
import { DeviceScreen } from "@/components/device-screen";
import { Spinner, Callout, Button } from "@/components/ui";
import { getDevice, type Device } from "@/lib/api/devices";

export default function ScreenWindow() {
  const { id } = useParams<{ id: string }>();
  const router = useRouter();
  const { principal, loading } = useSession();
  const [device, setDevice] = useState<Device | null>(null);
  const [error, setError] = useState<unknown>(null);

  useEffect(() => {
    if (loading) return;
    if (!principal) {
      router.replace("/login");
      return;
    }
    getDevice(id).then(setDevice).catch(setError);
  }, [id, loading, principal, router]);

  useEffect(() => {
    if (device) document.title = `${device.hostname || "Device"} — Remote screen`;
  }, [device]);

  if (loading || (!device && !error)) {
    return <Spinner className="min-h-screen items-center" />;
  }
  if (error || !device) {
    return (
      <div className="flex min-h-screen items-center justify-center p-6">
        <Callout tone="danger" title="Cannot open the screen">
          {error instanceof Error ? error.message : "The device could not be loaded."}
        </Callout>
      </div>
    );
  }
  if (!device.screen_sharing_enabled) {
    return (
      <div className="flex min-h-screen flex-col items-center justify-center gap-3 p-6 text-center">
        <Callout tone="warning" title="Remote screen is not enabled for this device">
          Enable it from the device page first.
        </Callout>
        <Button onClick={() => window.close()}>Close window</Button>
      </div>
    );
  }

  return (
    <div className="h-screen w-screen overflow-hidden">
      <DeviceScreen deviceId={device.id} hostname={device.hostname || device.serial_number} standalone />
    </div>
  );
}
