mock_provider "aws" {}
variables {
  account_id        = "123456789012"
  region            = "ap-southeast-2"
  installation_name = "miles-member-test"
  environment       = "test"
  owner             = "member"
  operator_user_arn = "arn:aws:iam::123456789012:user/miles/operators/member-deploy"
  deploy_role_arn   = "arn:aws:iam::123456789012:role/miles/bootstrap/member-deploy"
  boundary_arn      = "arn:aws:iam::123456789012:policy/miles/member/RuntimeBoundary"
  boundary_document = "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Action\":\"sts:GetCallerIdentity\",\"Resource\":\"*\"}]}"
  user_document     = "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Action\":\"iam:ChangePassword\",\"Resource\":\"*\"}]}"
  trust_document    = "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Action\":\"sts:AssumeRole\",\"Principal\":{\"AWS\":\"arn:aws:iam::123456789012:user/miles/operators/member-deploy\"},\"Condition\":{\"Bool\":{\"aws:MultiFactorAuthPresent\":\"true\"}}}]}"
  deploy_documents = {
    "01" = "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Action\":\"sts:GetCallerIdentity\",\"Resource\":\"*\"}]}"
  }
}
run "member_identity_paths" {
  command = plan
  assert {
    condition     = aws_iam_user.operator.name == "member-deploy" && aws_iam_user.operator.path == "/miles/operators/" && aws_iam_role.deploy.path == "/miles/bootstrap/" && aws_iam_policy.boundary.path == "/miles/member/"
    error_message = "All bootstrap identities must retain the configured names and IAM paths."
  }
  assert {
    condition     = jsondecode(aws_iam_role.deploy.assume_role_policy).Statement[0].Condition.Bool["aws:MultiFactorAuthPresent"] == "true" && aws_iam_role.deploy.max_session_duration == 3600
    error_message = "Operator access requires MFA and bounded role sessions."
  }
}
