#!/bin/sh

# 环境变量
ANONYMIZE_MODE=${ANONYMIZE_MODE:-partial}
FILTER_WORDS=${FILTER_WORDS:-}
LOG_FILE=${LOG_FILE:-/var/log/0.log}

# 构建关键词过滤规则
KEYWORD_SED=""
for w in $FILTER_WORDS; do
    KEYWORD_SED="${KEYWORD_SED}s/$w//g;"
done

# 根据模式选择IP处理方式
case "$ANONYMIZE_MODE" in
    hash)
        PROCESS_PIPELINE="perl -pe '
            s/\b(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})\b/
                my \$h = 0;
                \$h = (\$h >> 8) ^ (0xEDB88320 & ~((\$h & 0xFF) ^ ord(\$_))) for split //, \$1;
                sprintf(\"[H:%04X]\", ~\$h & 0xFFFF);
            /ge;
            ${KEYWORD_SED}
        '"
        ;;
    partial)
        PROCESS_PIPELINE="sed -E 's/\b(([0-9]{1,3}\.){3})[0-9]{1,3}\b/\1xxx/g;${KEYWORD_SED}'"
        ;;
    full|*)
        PROCESS_PIPELINE="sed -E 's/\b([0-9]{1,3}\.){3}[0-9]{1,3}\b/xxx.xxx.xxx.xxx/g;${KEYWORD_SED}'"
        ;;
esac

tmux new-session -d -s logview -n log \
  "tail -n 100 -f '$LOG_FILE' | $PROCESS_PIPELINE | ccze -A"

tmux set-option -t logview -g window-size smallest
tmux set-option -t logview -g prefix None
tmux set-option -t logview -g mouse off
tmux set-option -t logview -g key-table off
tmux set-option -t logview -g set-titles off
tmux set-option -t logview -g allow-rename off
tmux set-option -t logview -g history-limit 100
tmux set-window-option -t logview -g monitor-activity off

exec ttyd \
  --port 7681 \
  --ping-interval 30 \
  --max-clients 500 \
  -d "${LWS_LOG_LEVEL:-0}" \
  -T screen-256color \
  tmux attach -t logview -r
