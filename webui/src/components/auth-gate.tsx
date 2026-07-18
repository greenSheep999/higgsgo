import { useEffect, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { admin, ApiError } from "@/lib/api";
import {
  getBearer,
  setBearer,
  clearBearer,
  subscribeBearer,
} from "@/lib/auth-store";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Card,
  CardContent,
  CardDescription,
  CardFooter,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";

// AuthGate is a full-viewport bearer prompt. It doesn't render children until
// the token in localStorage has been verified against /admin/stats/pool.
// A 401 anywhere else in the app clears the token (see api.ts) which will
// cause this component to re-mount the login form.
export function AuthGate({ children }: { children: ReactNode }) {
  const [phase, setPhase] = useState<"checking" | "prompt" | "ready">(
    getBearer() ? "checking" : "prompt",
  );

  useEffect(() => {
    if (phase !== "checking") return;
    let cancelled = false;
    (async () => {
      try {
        await admin.ping();
        if (!cancelled) setPhase("ready");
      } catch {
        if (!cancelled) {
          clearBearer();
          setPhase("prompt");
        }
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [phase]);

  useEffect(() => {
    return subscribeBearer(() => {
      if (!getBearer()) setPhase("prompt");
    });
  }, []);

  return <AuthGateInner phase={phase} setPhase={setPhase}>{children}</AuthGateInner>;
}

function AuthGateInner({
  phase,
  setPhase,
  children,
}: {
  phase: "checking" | "prompt" | "ready";
  setPhase: (p: "checking" | "prompt" | "ready") => void;
  children: ReactNode;
}) {
  const { t } = useTranslation();
  if (phase === "checking") {
    return (
      <div className="grid min-h-svh place-items-center text-muted-foreground">
        {t("common.signInVerifying")}
      </div>
    );
  }
  if (phase === "prompt") {
    return (
      <LoginScreen
        onSuccess={() => {
          setPhase("ready");
        }}
      />
    );
  }
  return <>{children}</>;
}

function LoginScreen({ onSuccess }: { onSuccess: () => void }) {
  const { t } = useTranslation();
  const [token, setToken] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    setBearer(token.trim());
    try {
      await admin.ping();
      onSuccess();
    } catch (err) {
      clearBearer();
      const msg =
        err instanceof ApiError
          ? `${err.status} ${err.type}: ${err.message}`
          : err instanceof Error
            ? err.message
            : "Login failed";
      setError(msg);
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="grid min-h-svh place-items-center bg-muted px-4">
      <Card className="w-full max-w-sm">
        <CardHeader>
          <CardTitle>{t("common.appName")}</CardTitle>
          <CardDescription>{t("common.signInHint")}</CardDescription>
        </CardHeader>
        <form onSubmit={submit}>
          <CardContent className="space-y-3">
            <div className="space-y-2">
              <Label htmlFor="bearer">{t("common.bearerToken")}</Label>
              <Input
                id="bearer"
                type="password"
                autoComplete="off"
                value={token}
                onChange={(e) => setToken(e.target.value)}
                required
              />
            </div>
            {error ? (
              <p className="text-sm text-destructive">{error}</p>
            ) : null}
          </CardContent>
          <CardFooter>
            <Button
              type="submit"
              className="w-full"
              disabled={busy || !token.trim()}
            >
              {busy ? t("common.signInVerifying") : t("common.signIn")}
            </Button>
          </CardFooter>
        </form>
      </Card>
    </div>
  );
}
