import i18n from "i18next";
import LanguageDetector from "i18next-browser-languagedetector";
import { initReactI18next } from "react-i18next";
import en from "@/locales/en";
import zh from "@/locales/zh";

// i18n bootstrap. Two locales for now — en (source of truth) and zh
// (Simplified Chinese) — with the language detector deciding based on
// (in order) explicit user pick from localStorage, ?lng= query, and the
// browser's Accept-Language. The choice sticks via the "i18nextLng"
// localStorage key that i18next-browser-languagedetector manages.

void i18n
  .use(LanguageDetector)
  .use(initReactI18next)
  .init({
    resources: {
      en: { translation: en },
      zh: { translation: zh },
    },
    fallbackLng: "en",
    supportedLngs: ["en", "zh"],
    interpolation: { escapeValue: false },
    detection: {
      order: ["localStorage", "querystring", "navigator"],
      caches: ["localStorage"],
    },
    // React 19 with react-i18next 17 will otherwise throw a promise on
    // first render (Suspense-style) and — since we don't wrap the tree
    // in <Suspense> — crashes the app with "Cannot read useContext of
    // null". Everything is bundled into memory, so init is effectively
    // sync anyway; disabling suspense is the right knob.
    react: { useSuspense: false },
  });

export default i18n;
