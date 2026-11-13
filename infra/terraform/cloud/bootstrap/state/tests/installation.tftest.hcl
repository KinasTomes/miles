mock_provider "aws" {}

variables {
  account_id = "123456789012"
}

run "legacy_backend" {
  command = plan
  assert {
    condition     = aws_s3_bucket.state.bucket == "miles-dev-tfstate-123456789012-ap-southeast-1"
    error_message = "Existing state bucket names must remain stable."
  }
}

run "member_backend" {
  command = plan
  variables {
    account_id        = "210987654321"
    region            = "ap-southeast-2"
    installation_name = "miles-member-test"
    environment       = "test"
    owner             = "Member"
  }
  assert {
    condition     = aws_s3_bucket.state.bucket == "miles-member-test-tfstate-210987654321-ap-southeast-2"
    error_message = "State bucket name must use the same installation inputs as its policies."
  }
}
