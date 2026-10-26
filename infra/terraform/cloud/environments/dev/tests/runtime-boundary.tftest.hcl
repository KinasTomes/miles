mock_provider "aws" {}

run "every_runtime_role_is_bounded" {
  command = plan
  module { source = "../../modules/iam" }
  variables {
    name                     = "miles-dev"
    region                   = "ap-southeast-1"
    account_id               = "123456789012"
    data_bucket_name         = "miles-dev-data-123456789012-ap-southeast-1"
    table_arns               = ["arn:aws:dynamodb:ap-southeast-1:123456789012:table/miles-dev-metadata"]
    kms_key_arn              = "arn:aws:kms:ap-southeast-1:123456789012:key/example"
    secret_arn               = "arn:aws:secretsmanager:ap-southeast-1:123456789012:secret:miles-dev/runtime-secrets-example"
    parameter_arn            = "arn:aws:ssm:ap-southeast-1:123456789012:parameter/miles-dev/bootstrap-config"
    log_group_arn            = "arn:aws:logs:ap-southeast-1:123456789012:log-group:/miles-dev/runtime"
    permissions_boundary_arn = "arn:aws:iam::123456789012:policy/miles/bootstrap/MilesDevRuntimeBoundary"
  }
  assert {
    condition     = length(aws_iam_role.runtime) == 2 && alltrue([for role in aws_iam_role.runtime : role.permissions_boundary == var.permissions_boundary_arn])
    error_message = "Both EC2 runtime roles must retain the configured permissions boundary."
  }
}
