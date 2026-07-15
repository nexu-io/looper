import { Button } from "@/components/ui/button";

export function PanelError({
  message,
  onRetry,
}: {
  message: string;
  onRetry?: () => void;
}) {
  return (
    <div className="flex flex-col gap-2 py-1">
      <p className="m-0 text-[12px] text-[var(--danger)]">{message}</p>
      {onRetry ? (
        <div>
          <Button variant="ghost" size="sm" onClick={onRetry}>
            Retry
          </Button>
        </div>
      ) : null}
    </div>
  );
}
