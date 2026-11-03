mock_provider "aws" {}
run "fresh_secret_avoids_scheduled_deletion" {
  command = plan
  module { source = "../../modules/security" }
  variables {
    name                = "miles-dev"
    region              = "ap-southeast-1"
    account_id          = "123456789012"
    runtime_secret_name = "miles-dev/runtime-secrets-shared"
  }
  assert {
    condition     = aws_secretsmanager_secret.runtime.name == "miles-dev/runtime-secrets-shared" && aws_secretsmanager_secret.runtime.recovery_window_in_days == 30
    error_message = "A fresh secret name must preserve the deletion recovery window."
  }
}
run "existing_secret_name_unchanged" {
  command = plan
  module { source = "../../modules/security" }
  variables {
    name       = "miles-dev"
    region     = "ap-southeast-1"
    account_id = "123456789012"
  }
  assert {
    condition     = aws_secretsmanager_secret.runtime.name == "miles-dev/runtime-secrets"
    error_message = "Existing configurations must not rename or replace their secret."
  }
}
