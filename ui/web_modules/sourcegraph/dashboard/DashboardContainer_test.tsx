// tslint:disable: typedef ordered-imports curly

import {autotest} from "sourcegraph/util/autotest";

import * as React from "react";
import {DashboardContainer} from "sourcegraph/dashboard/DashboardContainer";
import {withUserContext} from "sourcegraph/app/user";
import testdataData from "sourcegraph/dashboard/testdata/DashboardContainer-data.json";

describe("DashboardContainer", () => {
	it("should render a dashboard", () => {
		autotest(testdataData, "sourcegraph/dashboard/testdata/DashboardContainer-data.json",
			React.createElement(withUserContext(<DashboardContainer />)),
			{
				siteConfig: {},
				signedIn: false,
				user: null,
				githubToken: null,
				eventLogger: {logEvent: () => null},
			},
		);
	});
});
