mock_provider "aws" {}
variables {
  account_id = "123456789012"
  paused     = false
}
run "alarms_opt_in" {
  command = plan
  assert {
    condition     = length(output.alarm_contract) == 0
    error_message = "Default dev must not create paid runtime alarms."
  }
}
run "cloud_edge_alarms" {
  command = plan
  variables { enable_alarms = true }
  assert {
    condition     = length(output.alarm_contract) == 10 && output.alarm_contract["cloud-recovery_cut_age"].threshold == 3600 && output.alarm_contract["edge-integrity"].statistic == "Sum"
    error_message = "Both profiles need cut/integrity/throttle/Completing/freshness alarms."
  }
  assert {
    condition     = output.alarm_contract["edge-completing"].dimensions == tomap({ profile = "edge", installation = "miles-dev-" }) && output.alarm_contract["cloud-recovery_cut_age"].missing == "breaching" && output.alarm_contract["edge-throttling"].missing == "notBreaching"
    error_message = "Alarm dimensions must match publisher; absent gauge observations must fail closed."
  }
}
run "cloud_only" {
  command = plan
  variables {
    enable_alarms      = true
    monitored_profiles = ["cloud"]
  }
  assert {
    condition     = length(output.alarm_contract) == 5
    error_message = "Do not monitor an absent Edge profile."
  }
}
run "pause_removes_alarms" {
  command = plan
  variables {
    enable_alarms = true
    paused        = true
  }
  assert {
    condition     = length(output.alarm_contract) == 0
    error_message = "Paused environments must not retain paid runtime alarms."
  }
}
