mock_provider "aws" {
  mock_resource "aws_rolesanywhere_trust_anchor" {
    defaults = { arn = "arn:aws:rolesanywhere:ap-southeast-1:123456789012:trust-anchor/00000000-0000-0000-0000-000000000001" }
  }
}
run "separate_edge_identity" {
  command = apply
  module { source = "../../modules/iam" }
  variables {
    name                     = "miles-dev"
    region                   = "ap-southeast-1"
    account_id               = "123456789012"
    data_bucket_name         = "miles-fixture"
    table_arns               = ["arn:aws:dynamodb:ap-southeast-1:123456789012:table/miles-fixture"]
    kms_key_arn              = "arn:aws:kms:ap-southeast-1:123456789012:key/00000000-0000-0000-0000-000000000001"
    secret_arn               = "arn:aws:secretsmanager:ap-southeast-1:123456789012:secret:runtime-fixture"
    parameter_arn            = "arn:aws:ssm:ap-southeast-1:123456789012:parameter/miles-fixture"
    log_group_arn            = "arn:aws:logs:ap-southeast-1:123456789012:log-group:/miles-fixture/runtime"
    permissions_boundary_arn = "arn:aws:iam::123456789012:policy/runtime-boundary"
    edge_ca_pem              = "mock-public-ca; mock provider does not validate a real certificate"
  }
  assert {
    condition     = length(aws_iam_role.runtime) == 3 && length(aws_iam_instance_profile.runtime) == 2 && aws_rolesanywhere_profile.edge[0].duration_seconds == 900 && aws_rolesanywhere_profile.edge[0].role_arns == toset([aws_iam_role.runtime["edge"].arn])
    error_message = "Edge gets a separate temporary provider role, never an EC2 profile or access key."
  }
  assert {
    condition     = jsondecode(aws_iam_role.runtime["edge"].assume_role_policy).Statement[0].Condition.ArnEquals["aws:SourceArn"] == aws_rolesanywhere_trust_anchor.edge[0].arn && jsondecode(aws_iam_role.runtime["edge"].assume_role_policy).Statement[0].Condition.StringEquals["aws:PrincipalTag/x509Subject/CN"] == "miles-edge"
    error_message = "Edge trust must bind the anchor, account and certificate subject."
  }
  assert {
    condition     = length([for s in jsondecode(aws_iam_role_policy.runtime["edge"].policy).Statement : s if contains(["CleanupData", "OwnFleetLifecycle", "DrainOwnFleetTargets", "RegisterDevNode"], s.Sid)]) == 0
    error_message = "Edge cannot delete physical data, manage the fleet or impersonate its SSM agent."
  }
}
