import { useState } from "react";
import { Button } from "@/components/ui/button";

async function copyText(text: string): Promise<void> {
  try {
    await navigator.clipboard.writeText(text);
  } catch {
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.style.position = "fixed";
    ta.style.left = "-9999px";
    document.body.appendChild(ta);
    ta.select();
    const ok = document.execCommand("copy");
    document.body.removeChild(ta);
    if (!ok) {
      throw new Error("Copy failed");
    }
  }
}

export function CopyButton({
  text,
  label = "Copy",
  className = "",
}: {
  text: string;
  label?: string;
  className?: string;
}) {
  const [copied, setCopied] = useState(false);

  const onClick = async () => {
    if (!text) return;
    try {
      await copyText(text);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    } catch {
      // ignore
    }
  };

  return (
    <Button
      variant="ghost"
      size="sm"
      className={className}
      onClick={() => void onClick()}
      disabled={!text}
      title={text || undefined}
    >
      {copied ? "Copied" : label}
    </Button>
  );
}
