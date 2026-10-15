import type * as React from "react";

import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";

/** Service credentials are not ARTEX account passwords. Chrome may ignore
 * autocomplete=off on password inputs and replace an API key with the saved
 * login password. Keep these as text controls, visually masked in Chromium,
 * and opt out of browser/password-manager autofill. This is display masking,
 * not encryption; never use it for the actual login/change-password forms. */
function SecretInput({ className, ...props }: Omit<React.ComponentProps<"input">,
  "type" | "autoComplete" | "autoCapitalize" | "autoCorrect" | "spellCheck"
>) {
  return (
    <Input
      {...props}
      name={props.name ?? (props.id ? `artex-service-${props.id}` : undefined)}
      type="text"
      autoComplete="off"
      autoCapitalize="off"
      autoCorrect="off"
      spellCheck={false}
      data-sensitive-input="true"
      data-lpignore="true"
      data-1p-ignore="true"
      data-bwignore="true"
      className={cn("[-webkit-text-security:disc]", className)}
    />
  );
}

export { SecretInput };
