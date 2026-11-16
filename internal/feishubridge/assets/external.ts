// Looper's small dispatch seam. Rendering and conversation handling stay upstream.
export interface ExternalTask {
  chatId: string;
  threadId?: string;
  rootMessageId: string;
  cwd: string;
  title: string;
  prompt: string;
  requesterOpenId?: string;
  backendId?: string;
  model?: string;
}

export interface TurnEvent {
  phase: 'started' | 'bound' | 'finished' | 'failed';
  rootMessageId: string;
  chatId: string;
  threadId?: string;
  sessionId?: string;
  cwd: string;
  terminal?: string;
  text?: string;
}

export interface ExternalHooks {
  onTurn?: (event: TurnEvent) => Promise<void>;
}
