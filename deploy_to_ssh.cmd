@(REM coding:CP866
SETLOCAL ENABLEEXTENSIONS
SET "GOOS=linux"
SET "GOARCH=amd64"

SET "HOST=%~1"
)
@(IF "%HOST%"=="" (
    echo Usage: deploy_to_ssh.cmd ^<host^>
    echo   host: user@hostname or hostname
    exit /b 1
)

go build -o telegram-monitor.elf || EXIT /B
)
ssh %HOST% "cat > /tmp/telegram-monitor && chmod +x /tmp/telegram-monitor" <telegram-monitor.elf || EXIT /B
DEL telegram-monitor.elf
ssh %HOST% "cat > /tmp/telegram-monitor.service.template" <telegram-monitor.service.template || EXIT /B
REM Install the binary and systemd service on the remote host.
ssh %HOST% "sudo mv /tmp/telegram-monitor /usr/local/bin/telegram-monitor && sudo envsubst </tmp/telegram-monitor.service.template | sudo tee /etc/systemd/system/telegram-monitor.service >/dev/null && sudo systemctl daemon-reload && sudo systemctl enable telegram-monitor.service && sudo systemctl start telegram-monitor.service"
