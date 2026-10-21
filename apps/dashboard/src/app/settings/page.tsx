import { redirect } from "next/navigation";

/** `/settings` on its own has nothing to show; the first section does. */
export default function SettingsIndex() {
  redirect("/settings/general");
}
