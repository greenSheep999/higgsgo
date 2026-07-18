import { useTranslation } from "react-i18next";
import { IconLanguage } from "@tabler/icons-react";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";

// Language dropdown — sits next to the theme toggle in the header. The
// active language name is not shown on the trigger to keep the icon-only
// symmetry with the theme toggle; a checkmark on the current menu item
// makes the selection legible without adding trigger text.

export function LanguageToggle() {
  const { i18n, t } = useTranslation();
  const current = i18n.resolvedLanguage;
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="ghost" size="icon">
          <IconLanguage className="size-4" />
          <span className="sr-only">{t("common.language.toggle")}</span>
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        <DropdownMenuItem
          onClick={() => i18n.changeLanguage("en")}
          data-active={current === "en"}
          className="data-[active=true]:font-semibold"
        >
          {t("common.language.en")}
        </DropdownMenuItem>
        <DropdownMenuItem
          onClick={() => i18n.changeLanguage("zh")}
          data-active={current === "zh"}
          className="data-[active=true]:font-semibold"
        >
          {t("common.language.zh")}
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
