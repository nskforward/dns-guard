#!/usr/bin/expect -f
set timeout 300
set host $env(MIKROTIK_HOST)
set user $env(MIKROTIK_USER_NAME)
set pass $env(MIKROTIK_USER_PASSWORD)
set cmd [lindex $argv 0]
log_user 1
spawn ssh -o StrictHostKeyChecking=accept-new -o ConnectTimeout=10 "$user@$host" $cmd
expect {
  -re "(?i)password:" { send "$pass\r"; exp_continue }
  eof
}
catch wait result
exit [lindex $result 3]
