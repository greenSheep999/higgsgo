import { createRoot } from "react-dom/client";
import { RouterProvider } from "@tanstack/react-router";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import "./index.css";
import { router } from "@/router";
import { ThemeProvider } from "@/components/theme-provider";
import "@/lib/i18n";

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      retry: 1,
      staleTime: 5_000,
      refetchOnWindowFocus: false,
    },
  },
});

// Note: no <StrictMode> — with React 19 + next-themes 0.4.6 the double
// initial commit under strict mode triggered a Cannot-read-useContext
// crash the first paint. next-themes internally reads document.classList
// during its provider's first effect, and in strict mode the second run
// received a null context. We can revisit strict mode once next-themes
// ships a React 19-native release.
createRoot(document.getElementById("root")!).render(
  <ThemeProvider>
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  </ThemeProvider>,
);
