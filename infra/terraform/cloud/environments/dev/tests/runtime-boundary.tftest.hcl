mock_provider "aws" {}

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

run "every_runtime_role_is_bounded" {
  command = plan
  module { source = "../../modules/iam" }
  assert {
    condition     = length(aws_iam_role.runtime) == 2 && alltrue([for role in aws_iam_role.runtime : role.permissions_boundary == var.permissions_boundary_arn])
    error_message = "Both EC2 runtime roles must retain the configured permissions boundary."
  }
}
run "fleet_gateway_permissions" {
  command = plan
  module { source = "../../modules/iam" }
  variables {
    fleet_enabled  = true
    release_sha256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  }
  assert {
    condition = alltrue([
      for s in jsondecode(aws_iam_role_policy.runtime["gateway"].policy).Statement :
      !contains(s.Action, "s3:DeleteObject")
    ])
    error_message = "Lifecycle control does not grant gateway physical data deletion."
  }
  assert {
    condition = length([
      for s in jsondecode(aws_iam_role_policy.runtime["gateway"].policy).Statement :
      s if try(s.Sid == "OwnFleetLifecycle" && endswith(s.Resource, "autoScalingGroupName/miles-dev-gateway"), false)
      ]) == 1 && length([
      for s in jsondecode(aws_iam_role_policy.runtime["gateway"].policy).Statement :
      s if try(s.Sid == "ReadPinnedRelease" && endswith(s.Resource, "/releases/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/*"), false)
    ]) == 1
    error_message = "Gateway must be limited to its own ASG and pinned release prefix."
  }
}

run "physical_inventory_permissions" {
  command = plan
  module { source = "../../modules/iam" }
  variables {
    edge_ca_pem = "mock-public-ca; mock provider does not validate a real certificate"
  }
  assert {
    condition = length(aws_iam_role_policy.runtime) == 3 && alltrue([
      for policy in aws_iam_role_policy.runtime : length([
        for statement in jsondecode(policy.policy).Statement : statement
        if try(statement.Sid == "ListData" && contains(statement.Action, "s3:ListBucketVersions") && statement.Resource == "arn:aws:s3:::${var.data_bucket_name}", false)
      ]) == 1
    ])
    error_message = "Cloud, Edge and jobs need version inventory permission on their data bucket."
  }
  assert {
    condition = length([
      for statement in jsondecode(file("${path.module}/../../bootstrap/operator/runtime-boundary.json.tmpl")).Statement : statement
      if statement.Sid == "ListDevData" && contains(statement.Action, "s3:ListBucketVersions") && statement.Resource == ["arn:aws:s3:::miles-dev-data-ACCOUNT_ID-ap-southeast-1"]
    ]) == 1
    error_message = "The operator bootstrap boundary must permit the same bucket-scoped version inventory."
  }
}
