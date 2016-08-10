// tslint:disable: typedef ordered-imports curly

import {rel} from "sourcegraph/app/routePatterns";
import {DashboardContainer} from "sourcegraph/dashboard/DashboardContainer";

export const routes: any[] = [
	{
		getComponent: (location, callback) => {
			callback(null, {
				main: DashboardContainer,
				navContext: null,
			});
		},
		path: rel.home,
	},
];
