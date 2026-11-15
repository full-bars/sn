# Systemd shakedown

The pre-release shakedown tests a systemd hotswap with RAM logs on and off. It waits for the old process to drain and checks that the successor stays active. The test unit has its start limit disabled for the rapid restart checks.
