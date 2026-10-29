mock_provider "aws" {}

run "only_own_autoscaling_role_can_use_encrypted_node_disks" {
  command = plan
  module { source = "../../modules/security" }
  variables {
    name                 = "miles-dev"
    region               = "ap-southeast-1"
    account_id           = "123456789012"
    autoscaling_role_arn = "arn:aws:iam::123456789012:role/aws-service-role/autoscaling.amazonaws.com/AWSServiceRoleForAutoScaling_miles-dev"
  }
  assert {
    condition = length([
      for statement in jsondecode(aws_kms_key.runtime.policy).Statement : statement
      if try(statement.Sid == "AutoScalingEBSGrants" &&
        statement.Principal.AWS == var.autoscaling_role_arn &&
      statement.Condition.Bool["kms:GrantIsForAWSResource"] == "true", false)
    ]) == 1
    error_message = "Encrypted EBS grants must be restricted to this ASG service-linked role and AWS resources."
  }
}
