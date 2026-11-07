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
run "cloud_edge_shallow_alarms" {
  command = plan
  variables { enable_alarms = true }
  assert {
    condition     = length(output.alarm_contract) == 8 && output.alarm_contract["cloud-recovery_cut_age"].threshold == 3600 && output.alarm_contract["edge-integrity"].statistic == "Sum"
    error_message = "Shallow observation creates cut/integrity/throttle/freshness alarms without completing."
  }
  assert {
    condition     = !contains(keys(output.alarm_contract), "cloud-completing") && !contains(keys(output.alarm_contract), "edge-completing") && output.alarm_contract["cloud-recovery_cut_age"].missing == "breaching" && output.alarm_contract["edge-throttling"].missing == "notBreaching" && output.alarm_contract["cloud-telemetry_age"].missing == "breaching"
    error_message = "Shallow mode must not create completing alarms with missing=breaching when miles_stuck_completing_uploads is unmeasured."
  }
}
run "cloud_edge_deep_alarms" {
  command = plan
  variables {
    enable_alarms    = true
    deep_observation = true
  }
  assert {
    condition     = length(output.alarm_contract) == 10 && output.alarm_contract["cloud-recovery_cut_age"].threshold == 3600 && output.alarm_contract["edge-integrity"].statistic == "Sum"
    error_message = "Deep observation creates cut/integrity/throttle/Completing/freshness alarms for both profiles."
  }
  assert {
    condition     = output.alarm_contract["edge-completing"].dimensions == tomap({ profile = "edge", installation = "miles-dev-" }) && output.alarm_contract["cloud-completing"].metric == "miles_stuck_completing_uploads" && output.alarm_contract["cloud-completing"].missing == "breaching" && output.alarm_contract["edge-completing"].missing == "breaching"
    error_message = "Deep observation creates completing alarm with missing=breaching and correct publisher dimensions."
  }
}
run "cloud_only_shallow" {
  command = plan
  variables {
    enable_alarms      = true
    monitored_profiles = ["cloud"]
  }
  assert {
    condition     = length(output.alarm_contract) == 4 && !contains(keys(output.alarm_contract), "cloud-completing")
    error_message = "Do not monitor an absent Edge profile, and do not create completing alarm without deep observation."
  }
}
run "cloud_only_deep" {
  command = plan
  variables {
    enable_alarms      = true
    monitored_profiles = ["cloud"]
    deep_observation   = true
  }
  assert {
    condition     = length(output.alarm_contract) == 5 && contains(keys(output.alarm_contract), "cloud-completing")
    error_message = "Deep observation on cloud-only profile creates 5 alarms including completing."
  }
}
run "pause_removes_alarms" {
  command = plan
  variables {
    enable_alarms    = true
    deep_observation = true
    paused           = true
  }
  assert {
    condition     = length(output.alarm_contract) == 0
    error_message = "Paused environments must not retain paid runtime alarms."
  }
}
