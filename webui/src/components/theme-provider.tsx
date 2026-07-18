import { ThemeProvider as NextThemesProvider } from "next-themes";
import type { ReactNode } from "react";

// Thin wrapper around next-themes so the rest of the app depends on a
// single import path. `attribute="class"` matches shadcn's convention of
// gating dark tokens with `.dark` on <html> (see the @custom-variant in
// src/index.css). `defaultTheme="system"` respects OS preference until
// the user manually flips the toggle, at which point next-themes stores
// the choice in localStorage.

export function ThemeProvider({ children }: { children: ReactNode }) {
  return (
    <NextThemesProvider
      attribute="class"
      defaultTheme="system"
      enableSystem
      disableTransitionOnChange
    >
      {children}
    </NextThemesProvider>
  );
}
