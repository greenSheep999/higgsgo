import { useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";
import { ApiError } from "@/lib/api";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import {
  Tabs,
  TabsContent,
  TabsList,
  TabsTrigger,
} from "@/components/ui/tabs";
import { Label } from "@/components/ui/label";

// ImportAccountsDialog wraps POST /admin/accounts. The endpoint accepts
// three JSON envelopes discriminated by the "format" field — see
// internal/api/admin/accounts.go for authoritative schemas. Rather than
// reinvent a form per format (they diverge sharply — session_paste is 12
// fields, higgsfield_register_json wraps an opaque blob under "raw", and
// raw_cookies takes a single Cookie header string) we let the operator
// paste a JSON body and validate it with a schema-agnostic JSON.parse.
// A "Fill template" button pre-seeds the textarea with the current tab's
// skeleton so a first-time user doesn't need to consult the doc.

const TEMPLATES: Record<string, string> = {
  session_paste: JSON.stringify(
    {
      format: "session_paste",
      email: "",
      user_id: "",
      session_id: "",
      workspace_id: "",
      cookies_json: [],
      user_agent: "",
      x_datadome_clientid: "",
      plan_type: "",
      credits_balance: 0,
      subscription_balance: 0,
      total_credits: 0,
      password: "",
    },
    null,
    2,
  ),
  higgsfield_register_json: JSON.stringify(
    {
      format: "higgsfield_register_json",
      raw: {
        type: "",
        email: "",
        password: "",
        user_id: "",
        session_id: "",
        plan_type: "",
        cookies: {},
        x_datadome_clientid: "",
        captured_user_agent: "",
        imported_at: "",
        credits_snapshot: {
          subscription_credits: 0,
          package_credits: 0,
          daily_credits: 0,
          total_plan_credits: 0,
          captured_at: "",
        },
      },
    },
    null,
    2,
  ),
  raw_cookies: JSON.stringify(
    {
      format: "raw_cookies",
      email: "",
      cookies_header: "",
      user_agent: "",
      x_datadome_clientid: "",
      plan_type: "",
      user_id: "",
    },
    null,
    2,
  ),
};

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onImported: () => void;
}

export function ImportAccountsDialog({ open, onOpenChange, onImported }: Props) {
  const { t } = useTranslation();
  const [format, setFormat] = useState<keyof typeof TEMPLATES>("session_paste");
  const [body, setBody] = useState("");
  const [upsert, setUpsert] = useState(false);

  const importer = useMutation({
    mutationFn: async () => {
      const parsed = JSON.parse(body) as Record<string, unknown>;
      if (!parsed.format) parsed.format = format;
      // upsert is a query-string flag on the server — we can't pass it via
      // the shared admin.importAccounts wrapper without leaking a param,
      // so we call fetch directly for this one flag.
      const bearer = localStorage.getItem("higgsgo.adminBearer");
      const res = await fetch(
        `/admin/accounts${upsert ? "?upsert=true" : ""}`,
        {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            Authorization: bearer ? `Bearer ${bearer}` : "",
          },
          body: JSON.stringify(parsed),
        },
      );
      const text = await res.text();
      const json = text ? JSON.parse(text) : null;
      if (!res.ok) {
        throw new ApiError(
          res.status,
          json?.error?.type ?? "http_error",
          json?.error?.message ?? `HTTP ${res.status}`,
        );
      }
      return json;
    },
    onSuccess: (data) => {
      toast.success(
        t("accounts.import.imported", { email: data?.email ?? "account" }),
      );
      onImported();
      onOpenChange(false);
      setBody("");
    },
    onError: (err) => {
      const msg =
        err instanceof ApiError
          ? `${err.status} ${err.type}: ${err.message}`
          : err instanceof Error
            ? err.message
            : t("accounts.import.failed");
      toast.error(msg);
    },
  });

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>{t("accounts.import.title")}</DialogTitle>
          <DialogDescription>
            {t("accounts.import.description")}
          </DialogDescription>
        </DialogHeader>

        <Tabs
          value={format}
          onValueChange={(v) => setFormat(v as keyof typeof TEMPLATES)}
        >
          <TabsList>
            <TabsTrigger value="session_paste">session_paste</TabsTrigger>
            <TabsTrigger value="higgsfield_register_json">
              higgsfield_register_json
            </TabsTrigger>
            <TabsTrigger value="raw_cookies">raw_cookies</TabsTrigger>
          </TabsList>
          {(["session_paste", "higgsfield_register_json", "raw_cookies"] as const).map(
            (f) => (
              <TabsContent key={f} value={f} className="space-y-2">
                <div className="flex items-center justify-between">
                  <Label>{t("accounts.import.jsonBody")}</Label>
                  <Button
                    variant="ghost"
                    size="sm"
                    type="button"
                    onClick={() => setBody(TEMPLATES[f])}
                  >
                    {t("accounts.import.fillTemplate")}
                  </Button>
                </div>
                <Textarea
                  value={body}
                  onChange={(e) => setBody(e.target.value)}
                  className="min-h-72 font-mono text-xs"
                  placeholder={TEMPLATES[f]}
                />
              </TabsContent>
            ),
          )}
        </Tabs>

        <div className="flex items-center gap-2 text-sm">
          <input
            id="upsert"
            type="checkbox"
            className="size-4"
            checked={upsert}
            onChange={(e) => setUpsert(e.target.checked)}
          />
          <Label htmlFor="upsert" className="cursor-pointer">
            {t("accounts.import.upsert")}
          </Label>
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            {t("common.cancel")}
          </Button>
          <Button
            onClick={() => importer.mutate()}
            disabled={!body.trim() || importer.isPending}
          >
            {importer.isPending
              ? t("accounts.import.importing")
              : t("common.import")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
